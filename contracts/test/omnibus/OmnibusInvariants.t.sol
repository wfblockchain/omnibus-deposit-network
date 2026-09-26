// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Test } from "forge-std/Test.sol";
import { StdInvariant } from "forge-std/StdInvariant.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { PaymentRouter } from "src/omnibus/PaymentRouter.sol";

/// @dev Drives random funding, minting, on-us transfers, cross-bank payments
///      (instant and accepted), rejections, expiries, redemptions, defunds
///      and interest across three banks. Tracks the Fed's view of the joint
///      account as a ghost: only Fedwire and Fed interest may change it.
contract OmnibusHandler is Test {

    uint256 constant M = 1e6;

    OmnibusLedger public ledger;
    PaymentRouter public router;
    BankToken[3] public tokens;
    bytes32[3] public ids;
    address[3] public operators;
    address[3] public issuers;
    address[3] public approvers;
    address[6] public holders; // two per bank, each admitted at every bank
    address public funding;

    uint256 public ghostFedBalance;
    uint256 public nonce;
    bytes32[] public pending;
    bytes32[] public defundIds;

    constructor(
        OmnibusLedger l,
        PaymentRouter r,
        BankToken[3] memory t,
        bytes32[3] memory i,
        address[3] memory ops,
        address[3] memory iss,
        address[6] memory hs,
        address f,
        uint256 fedStart,
        address[3] memory apps
    ) {
        approvers = apps;
        ledger = l;
        router = r;
        tokens = t;
        ids = i;
        operators = ops;
        issuers = iss;
        holders = hs;
        funding = f;
        ghostFedBalance = fedStart;
    }

    function _ref() internal returns (bytes32) {
        return keccak256(abi.encode(++nonce));
    }

    function fund(uint8 b, uint64 amt) external {
        uint256 a = bound(amt, 1, 1e9) * M;
        vm.prank(funding);
        ledger.creditFunding(ids[b % 3], a, _ref());
        ghostFedBalance += a;
    }

    function mint(uint8 b, uint8 h, uint64 amt) external {
        uint256 i = b % 3;
        uint256 freeP = ledger.freePosition(ids[i]);
        if (freeP == 0) return;
        uint256 a = bound(amt, 1, freeP);
        vm.prank(issuers[i]);
        tokens[i].mint(holders[h % 6], a, _ref());
    }

    function transferSame(uint8 b, uint8 from, uint8 to, uint64 amt) external {
        uint256 i = b % 3;
        address f = holders[from % 6];
        uint256 avail = tokens[i].availableBalanceOf(f);
        if (avail == 0) return;
        vm.prank(f);
        tokens[i].transfer(holders[to % 6], bound(amt, 1, avail));
    }

    function pay(uint8 fromB, uint8 toB, uint8 from, uint8 to, uint64 amt) external {
        uint256 i = fromB % 3;
        uint256 j = toB % 3;
        if (i == j) return;
        address f = holders[from % 6];
        uint256 avail = tokens[i].availableBalanceOf(f);
        if (avail == 0) return;
        vm.prank(f);
        (bytes32 pid, PaymentRouter.Status st) =
            router.pay(tokens[i], tokens[j], holders[to % 6], bound(amt, 1, avail), _ref());
        if (st == PaymentRouter.Status.Pending) pending.push(pid);
    }

    function answer(uint256 k, uint8 action) external {
        if (pending.length == 0) return;
        uint256 idx = k % pending.length;
        bytes32 pid = pending[idx];
        pending[idx] = pending[pending.length - 1];
        pending.pop();
        PaymentRouter.Payment memory p = router.payment(pid);
        if (p.status != PaymentRouter.Status.Pending) return;
        address op = ledger.member(ledger.memberOfToken(address(p.toToken))).operator;
        if (action % 3 == 0 && block.timestamp <= p.deadline) {
            vm.prank(op);
            router.accept(pid);
        } else if (action % 3 == 1) {
            vm.prank(op);
            router.reject(pid, "AC04");
        } else {
            vm.warp(uint256(p.deadline) + 1);
            router.expire(pid);
        }
    }

    function redeem(uint8 b, uint8 h, uint64 amt) external {
        uint256 i = b % 3;
        address who = holders[h % 6];
        uint256 avail = tokens[i].availableBalanceOf(who);
        if (avail == 0) return;
        vm.prank(who);
        tokens[i].redeem(bound(amt, 1, avail), _ref());
    }

    function requestDefund(uint8 b, uint64 amt) external {
        uint256 i = b % 3;
        uint256 freeP = ledger.freePosition(ids[i]);
        if (freeP == 0) return;
        uint256 cents = freeP / 1e4;
        if (cents == 0) return;
        vm.prank(operators[i]);
        bytes32 d = ledger.requestDefund(ids[i], bound(amt, 1, cents) * 1e4, _ref());
        vm.prank(approvers[i]);
        ledger.approveDefund(d);
        defundIds.push(d);
    }

    function settleDefund(uint256 k, bool ok) external {
        if (defundIds.length == 0) return;
        uint256 idx = k % defundIds.length;
        bytes32 d = defundIds[idx];
        defundIds[idx] = defundIds[defundIds.length - 1];
        defundIds.pop();
        (, uint256 amount, OmnibusLedger.DefundStatus st) = ledger.defunds(d);
        if (st != OmnibusLedger.DefundStatus.Approved) return;
        vm.prank(funding);
        if (ok) {
            ledger.confirmDefund(d, _ref());
            ghostFedBalance -= amount;
        } else {
            ledger.failDefund(d, "REJECTED");
        }
    }

    function interest(uint64 amt, uint32 dt) external {
        vm.warp(block.timestamp + bound(dt, 1, 30 days));
        uint256 a = bound(amt, 1, 1e6) * M;
        vm.prank(funding);
        ledger.distributeInterest(a, _ref());
        ghostFedBalance += a;
    }

    function pendingCount() external view returns (uint256) {
        return pending.length;
    }

}

contract OmnibusInvariantTest is StdInvariant, Test {

    uint256 constant M = 1e6;

    OmnibusLedger ledger;
    PaymentRouter router;
    OmnibusHandler handler;
    BankToken[3] tokens;

    function setUp() public {
        vm.warp(1_800_000_000);
        address funding = makeAddr("funding");
        ledger = new OmnibusLedger(address(this), 0);
        ledger.grantRole(ledger.GOVERNOR_ROLE(), address(this));
        ledger.grantRole(ledger.FUNDING_ROLE(), funding);
        router = new PaymentRouter(ledger, address(this), 0);
        ledger.grantRole(ledger.ROUTER_ROLE(), address(router));

        bytes32[3] memory ids = [bytes32("BANK_A"), bytes32("BANK_B"), bytes32("BANK_C")];
        address[3] memory ops = [makeAddr("op-bank-a"), makeAddr("op-bank-b"), makeAddr("op-bank-c")];
        address[3] memory iss = [makeAddr("iss-bank-a"), makeAddr("iss-bank-b"), makeAddr("iss-bank-c")];
        address[3] memory apps = [makeAddr("app-bank-a"), makeAddr("app-bank-b"), makeAddr("app-bank-c")];
        address[6] memory hs =
            [makeAddr("h0"), makeAddr("h1"), makeAddr("h2"), makeAddr("h3"), makeAddr("h4"), makeAddr("h5")];

        for (uint256 i = 0; i < 3; i++) {
            HolderRegistry reg = new HolderRegistry(address(this), 0);
            reg.grantRole(reg.REGISTRAR_ROLE(), address(this));
            for (uint256 h = 0; h < 6; h++) {
                reg.authorize(hs[h], "KYC");
            }
            tokens[i] = new BankToken("Bank USD", "bUSD", ledger, address(router), reg, address(this), 0);
            tokens[i].grantRole(tokens[i].ISSUER_ROLE(), iss[i]);
            ledger.admitMember(ids[i], address(tokens[i]), ops[i], apps[i], i == 1); // BANK_B accepts explicitly
            vm.prank(funding);
            ledger.creditFunding(ids[i], 1_000_000 * M, keccak256(abi.encode("seed", i)));
        }

        handler = new OmnibusHandler(ledger, router, tokens, ids, ops, iss, hs, funding, 3_000_000 * M, apps);
        targetContract(address(handler));
    }

    /// Only Fedwire and Fed interest move the joint account's balance.
    function invariant_LedgerTotalEqualsTheFedBalance() public view {
        assertEq(ledger.omnibusTotal(), handler.ghostFedBalance());
    }

    /// Σ positions + undistributed == total; backing == supply; backing +
    /// pending defunds never exceed a position.
    function invariant_LedgerInvariantsHold() public view {
        assertTrue(ledger.invariantsHold());
    }

    /// Every token in circulation is backed by reserves in the omnibus.
    function invariant_TokensNeverExceedReserves() public view {
        uint256 supply;
        for (uint256 i = 0; i < 3; i++) {
            supply += tokens[i].totalSupply();
        }
        assertLe(supply, ledger.omnibusTotal());
    }

}
