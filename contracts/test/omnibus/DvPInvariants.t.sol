// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Test } from "forge-std/Test.sol";
import { Vm } from "forge-std/Vm.sol";
import { DvPBase } from "./DvP.t.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { CrossChainMessenger } from "src/omnibus/crosschain/CrossChainMessenger.sol";
import { RemoteBankToken } from "src/omnibus/crosschain/RemoteBankToken.sol";
import { DvPSettlement } from "src/omnibus/dvp/DvPSettlement.sol";
import { MockSecurityToken } from "test/utils/MockSecurityToken.sol";

/// @dev Random sequences over the venue: trades matched, cash locked at home
///      and delivered (right amount, wrong amount, late), assets escrowed or
///      not, approvals given or not, settlement, bilateral cancellation,
///      lapse and refund, credits withdrawn, the buyer dropped from and
///      re-added to the bank's holder list, the seller frozen and unfrozen.
contract DvPHandler is Test {

    uint256 constant M = 1e6;
    uint256 constant SHARE = 1e18;
    bytes32 constant BANK_A = "BNKAUS30";

    DvPSettlement public dvp;
    CrossChainMessenger public home;
    CrossChainMessenger public remote;
    RemoteBankToken public cash;
    MockSecurityToken public tbill;
    HolderRegistry public reg;
    address public buyer;
    address public seller;
    uint256[2] keys;
    uint256 issuerKey;

    bytes32[] public ids;
    bytes[] public pending;
    uint256[] public pendingAmount;
    uint256 public inFlight;
    uint256 salt;

    constructor(
        DvPSettlement d,
        CrossChainMessenger h,
        CrossChainMessenger r,
        RemoteBankToken c,
        MockSecurityToken t,
        HolderRegistry g,
        address b,
        address s,
        uint256[2] memory k,
        uint256 ik
    ) {
        (dvp, home, remote, cash, tbill, reg, buyer, seller, keys, issuerKey) = (d, h, r, c, t, g, b, s, k, ik);
    }

    function idCount() external view returns (uint256) {
        return ids.length;
    }

    /// Half the time the newest trade, so sequences reach settlement often.
    function _id(uint256 k) internal view returns (bytes32) {
        return k % 2 == 0 ? ids[ids.length - 1] : ids[(k / 2) % ids.length];
    }

    function open(uint32 shares, uint32 dollars, uint16 life) external {
        DvPSettlement.Terms memory t = DvPSettlement.Terms({
            seller: seller,
            buyer: buyer,
            asset: address(tbill),
            assetAmount: bound(shares, 1, 1_000) * SHARE,
            cash: address(cash),
            cashAmount: bound(dollars, 1, 1_000_000) * M,
            settleBy: uint64(block.timestamp + bound(life, 60, 6 hours)),
            ref: bytes32(salt),
            salt: bytes32(++salt)
        });
        vm.prank(seller);
        bytes32 id = dvp.affirm(t);
        vm.prank(buyer);
        dvp.affirm(t);
        ids.push(id);
    }

    function approveSeller(uint256 k) external {
        if (ids.length == 0) return;
        // A standing approval half the time (how a seller that trades daily
        // runs), an exact one otherwise. Read first: a call eats the prank.
        uint256 amount = k % 2 == 0 ? type(uint256).max : dvp.trade(_id(k)).terms.assetAmount;
        vm.prank(seller);
        tbill.approve(address(dvp), amount);
    }

    function sendCash(uint256 k, bool exact) external {
        if (ids.length == 0) return;
        bytes32 id = _id(k);
        uint256 a = dvp.trade(id).terms.cashAmount - (exact ? 0 : 1);
        if (a == 0) return;
        vm.recordLogs();
        vm.prank(buyer);
        home.depositForBurnWithHook(BANK_A, a, 7, address(dvp), address(dvp), abi.encode(id, buyer), address(0));
        Vm.Log[] memory logs = vm.getRecordedLogs();
        for (uint256 i = logs.length; i > 0; i--) {
            if (logs[i - 1].topics[0] == keccak256("MessageSent(bytes)")) {
                pending.push(abi.decode(logs[i - 1].data, (bytes)));
                pendingAmount.push(a);
                inFlight += a;
                return;
            }
        }
    }

    function deliver(uint256 k) external {
        if (pending.length == 0) return;
        uint256 i = k % pending.length;
        bytes memory m = pending[i];
        uint256 a = pendingAmount[i];
        pending[i] = pending[pending.length - 1];
        pendingAmount[i] = pendingAmount[pendingAmount.length - 1];
        pending.pop();
        pendingAmount.pop();
        vm.recordLogs();
        (bytes[] memory sigs, bytes memory iss) = _sign(m);
        dvp.receiveCash(m, sigs, iss);
        inFlight -= a;
        // The relayer takes the mint's acknowledgement home: the escrow burns.
        Vm.Log[] memory logs = vm.getRecordedLogs();
        for (uint256 j = logs.length; j > 0; j--) {
            if (logs[j - 1].topics[0] == keccak256("MessageSent(bytes)")) {
                bytes memory ack = abi.decode(logs[j - 1].data, (bytes));
                (sigs, iss) = _sign(ack);
                home.receiveMessage(ack, sigs, iss);
                return;
            }
        }
        revert("no acknowledgement");
    }

    function _sign(bytes memory m) internal view returns (bytes[] memory sigs, bytes memory iss) {
        bytes32 digest = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", keccak256(m)));
        sigs = new bytes[](2);
        (uint256 k0, uint256 k1) = vm.addr(keys[0]) < vm.addr(keys[1]) ? (keys[0], keys[1]) : (keys[1], keys[0]);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(k0, digest);
        sigs[0] = abi.encodePacked(r, s, v);
        (v, r, s) = vm.sign(k1, digest);
        sigs[1] = abi.encodePacked(r, s, v);
        (v, r, s) = vm.sign(issuerKey, digest);
        iss = abi.encodePacked(r, s, v);
    }

    /// The newest trade's whole cash path in one step: standing approval,
    /// lock at home, delivery here. Interleaves settlement with the rest.
    function payNewest() external {
        if (ids.length == 0) return;
        vm.prank(seller);
        tbill.approve(address(dvp), type(uint256).max);
        this.sendCash(0, true);
        this.deliver(pending.length - 1);
    }

    function fundAsset(uint256 k) external {
        if (ids.length == 0) return;
        bytes32 id = _id(k);
        uint256 amount = dvp.trade(id).terms.assetAmount;
        vm.prank(seller);
        tbill.approve(address(dvp), amount);
        vm.prank(seller);
        try dvp.fundAsset(id) { } catch { }
    }

    function settle(uint256 k) external {
        if (ids.length == 0) return;
        try dvp.settle(_id(k)) { } catch { }
    }

    function cancelBoth(uint256 k) external {
        if (ids.length == 0) return;
        bytes32 id = _id(k);
        vm.prank(seller);
        try dvp.cancel(id) { } catch { }
        vm.prank(buyer);
        try dvp.cancel(id) { } catch { }
    }

    function refund(uint256 k) external {
        if (ids.length == 0) return;
        try dvp.refund(_id(k)) { } catch { }
    }

    function withdraw() external {
        vm.prank(buyer);
        try dvp.withdrawCredit(address(cash)) { } catch { }
    }

    function toggleBuyerAdmission(bool admit) external {
        if (admit) reg.authorize(buyer, "KYC");
        else reg.revoke(buyer, "LAPSED");
    }

    /// Frozen about one call in six, and fully: the seller cannot deliver.
    function freezeSeller(uint8 r) external {
        tbill.setFrozenTokens(seller, r < 40 ? tbill.balanceOf(seller) : 0);
    }

    function warp(uint16 s) external {
        vm.warp(block.timestamp + bound(s, 1, 45 minutes));
    }

}

contract DvPInvariantTest is DvPBase {

    DvPHandler handler;

    function setUp() public override {
        super.setUp();
        tbill.setEligible(address(dvp), true); // lets sellers escrow shares too
        handler = new DvPHandler(
            dvp, home, remote, cash, tbill, aRemoteRegistry, buyer, seller, [attesterKeys[0], attesterKeys[2]], aIssuerKey
        );
        aRemoteRegistry.grantRole(aRemoteRegistry.REGISTRAR_ROLE(), address(handler));
        tbill.setAgent(address(handler)); // the handler freezes as the transfer agent
        targetContract(address(handler));
    }

    /// The venue holds exactly what it owes: escrow plus credits, per token.
    function invariant_VenueHoldsExactlyWhatItOwes() public view virtual {
        assertEq(cash.balanceOf(address(dvp)), dvp.escrowed(address(cash)) + dvp.credited(address(cash)));
        assertEq(tbill.balanceOf(address(dvp)), dvp.escrowed(address(tbill)) + dvp.credited(address(tbill)));
    }

    /// Principal risk is zero: the buyer has shares only from settled trades
    /// and the seller has cash only from settled trades, amount for amount.
    function invariant_EverySettledTradeMovedBothLegsAndNothingElseMoved() public view virtual {
        uint256 shares;
        uint256 dollars;
        for (uint256 i = 0; i < handler.idCount(); i++) {
            DvPSettlement.Trade memory t = dvp.trade(handler.ids(i));
            if (t.status == DvPSettlement.Status.Settled) {
                shares += t.terms.assetAmount;
                dollars += t.terms.cashAmount;
            }
        }
        assertEq(tbill.balanceOf(buyer), shares, "buyer's shares == settled deliveries");
        assertEq(cash.balanceOf(seller), dollars, "seller's cash == settled payments");
    }

    /// Across both chains the backing never falls short: home counts as
    /// abroad exactly the tokens on domain 7, and cash not yet minted there
    /// sits in the home messenger's escrow, still part of home supply.
    function invariant_BackingCoversEveryTokenOnBothChains() public view virtual {
        assertEq(ledger.member(BANK_A).remoteSupply, cash.totalSupply());
        assertEq(dtA.balanceOf(address(home)), handler.inFlight());
        assertEq(home.escrowed(BANK_A), handler.inFlight());
        assertEq(home.outstanding(BANK_A, 7), ledger.member(BANK_A).remoteSupply);
        assertTrue(ledger.invariantsHold());
    }

}
