// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Test } from "forge-std/Test.sol";
import { StdInvariant } from "forge-std/StdInvariant.sol";
import { Vm } from "forge-std/Vm.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { OmnibusNetting } from "src/omnibus/OmnibusNetting.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { PaymentRouter } from "src/omnibus/PaymentRouter.sol";
import { CrossChainMessenger } from "src/omnibus/crosschain/CrossChainMessenger.sol";
import { RemoteBankToken } from "src/omnibus/crosschain/RemoteBankToken.sol";

/// @dev Everything at once, across three banks: minting, cross-bank payments,
///      banks holding and settling each other's tokens, gross and netted
///      obligations, Fedwire funding and defunds, interest, and tokens
///      travelling to a second chain and back with messages in flight.
/// Each bank's own attestation key, the same in the setup and the handler.
function issuerKeyOf(uint256 bank) pure returns (uint256) {
    return uint256(keccak256(abi.encode("issuer", bank))) % 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141;
}

contract NetworkHandler is Test {

    uint256 constant M = 1e6;

    OmnibusLedger public ledger;
    PaymentRouter public router;
    OmnibusNetting public netting;
    CrossChainMessenger public home;
    CrossChainMessenger public remote;
    BankToken[3] public tokens;
    RemoteBankToken[3] public remoteTokens;
    bytes32[3] public ids;
    address[3] public operators;
    address[3] public approvers;
    address[3] public issuers;
    address[3] public treasuries;
    address[4] public holders;
    address public funding;
    address public settlementOp;
    uint256[3] public attesterKeys;

    uint256 public ghostFed;
    uint256[3] public inFlight; // burned on one side, not yet minted on the other
    uint256 public nonce;
    bytes32[] public queued;

    struct Msg {
        bool toRemote;
        uint256 bank;
        uint256 amount;
        bytes body;
    }

    Msg[] public msgs;

    /// Actions that actually executed (not early returns), for coverage.
    mapping(bytes32 => uint256) public done;

    constructor(
        OmnibusLedger l,
        PaymentRouter r,
        OmnibusNetting n,
        CrossChainMessenger h,
        CrossChainMessenger rm,
        BankToken[3] memory t,
        RemoteBankToken[3] memory rt,
        bytes32[3] memory i,
        address[3][4] memory keys, // operators, approvers, issuers, treasuries
        address[4] memory hs,
        address f,
        address so,
        uint256[3] memory ak,
        uint256 fedStart
    ) {
        (ledger, router, netting, home, remote) = (l, r, n, h, rm);
        tokens = t;
        remoteTokens = rt;
        ids = i;
        operators = keys[0];
        approvers = keys[1];
        issuers = keys[2];
        treasuries = keys[3];
        holders = hs;
        funding = f;
        settlementOp = so;
        attesterKeys = ak;
        ghostFed = fedStart;
    }

    function _ref() internal returns (bytes32) {
        return keccak256(abi.encode("ref", ++nonce));
    }

    function fund(uint8 b, uint32 amt) external {
        uint256 a = bound(amt, 1, 1e6) * M;
        vm.prank(funding);
        ledger.creditFunding(ids[b % 3], a, _ref());
        ghostFed += a;
    }

    function mint(uint8 b, uint8 h, uint64 amt) external {
        uint256 i = b % 3;
        uint256 cap = ledger.mintCapacity(ids[i]);
        if (cap == 0) return;
        vm.prank(issuers[i]);
        tokens[i].mint(holders[h % 4], bound(amt, 1, cap), _ref());
    }

    function pay(uint8 fb, uint8 tb, uint8 h, uint8 to, uint64 amt) external {
        uint256 i = fb % 3;
        uint256 j = tb % 3;
        if (i == j) return;
        address f = holders[h % 4];
        uint256 avail = tokens[i].availableBalanceOf(f);
        if (avail == 0) return;
        vm.prank(f);
        router.pay(tokens[i], tokens[j], holders[to % 4], bound(amt, 1, avail), _ref());
        done["pay"]++;
    }

    function toTreasury(uint8 b, uint8 h, uint8 t, uint64 amt) external {
        uint256 i = b % 3;
        address f = holders[h % 4];
        uint256 avail = tokens[i].availableBalanceOf(f);
        if (avail == 0) return;
        vm.prank(f);
        tokens[i].transfer(treasuries[t % 3], bound(amt, 1, avail));
    }

    function settleHeld(uint8 b, uint8 t, uint64 amt) external {
        uint256 i = b % 3;
        uint256 j = t % 3;
        if (i == j) return;
        uint256 bal = tokens[i].availableBalanceOf(treasuries[j]);
        if (bal == 0) return;
        vm.prank(treasuries[j]);
        router.settleHeld(tokens[i], bound(amt, 1, bal));
        done["settleHeld"]++;
    }

    function submit(uint8 p, uint8 q, uint32 amt) external {
        uint256 i = p % 3;
        uint256 j = q % 3;
        if (i == j) return;
        vm.prank(operators[i]);
        // Sized so gross settlement is often fundable and netting often needed.
        queued.push(netting.submit(ids[i], ids[j], uint128(bound(amt, 1, 2e5) * M), _ref()));
    }

    function settleGross(uint256 k) external {
        if (queued.length == 0) return;
        // Cycles settle everything live, so pick among the newest submissions.
        uint256 span = queued.length < 3 ? queued.length : 3;
        bytes32 id = queued[queued.length - 1 - (k % span)];
        (bytes32 payer,, uint128 amount, uint64 exp, OmnibusNetting.Status st) = netting.obligations(id);
        if (st != OmnibusNetting.Status.Queued || block.timestamp > exp) return;
        if (ledger.freePosition(payer) < amount) return;
        uint256 i = payer == ids[0] ? 0 : payer == ids[1] ? 1 : 2;
        vm.prank(operators[i]);
        netting.settleGross(id);
        done["gross"]++;
    }

    /// Plans a cycle over every live queued obligation, as the operator's planner
    /// would, and submits it only if every net debit is fundable.
    function cycle() external {
        uint256 n;
        bytes32[] memory live = new bytes32[](queued.length);
        int256[3] memory net;
        for (uint256 k = 0; k < queued.length; k++) {
            (bytes32 payer, bytes32 payee, uint128 amount, uint64 exp, OmnibusNetting.Status st) = netting.obligations(queued[k]);
            if (st != OmnibusNetting.Status.Queued || block.timestamp > exp) continue;
            live[n++] = queued[k];
            net[_i(payer)] -= int256(uint256(amount));
            net[_i(payee)] += int256(uint256(amount));
        }
        if (n == 0) return;
        for (uint256 i = 0; i < 3; i++) {
            if (net[i] < 0 && uint256(-net[i]) > ledger.freePosition(ids[i])) return;
        }
        bytes32[] memory discharged = new bytes32[](n);
        for (uint256 k = 0; k < n; k++) {
            discharged[k] = live[k];
        }
        _sort(discharged);
        (bytes32[] memory members, int256[] memory nets) = _sortedMembers(net);
        vm.prank(settlementOp);
        netting.settleCycle(_ref(), members, nets, discharged);
        done["cycle"]++;
    }

    function defund(uint8 b, uint32 amt, bool ok) external {
        uint256 i = b % 3;
        uint256 cents = ledger.mintCapacity(ids[i]) / 1e4; // free above the floor
        if (cents == 0) return;
        uint256 a = bound(amt, 1, cents) * 1e4;
        vm.prank(operators[i]);
        bytes32 d = ledger.requestDefund(ids[i], a, _ref());
        vm.prank(approvers[i]);
        ledger.approveDefund(d);
        vm.prank(funding);
        if (ok) {
            ledger.confirmDefund(d, _ref());
            ghostFed -= a;
            done["defund"]++;
        } else {
            ledger.failDefund(d, "REJECTED");
        }
    }

    function interest(uint32 amt, uint32 dt) external {
        vm.warp(block.timestamp + bound(dt, 1, 3 days));
        uint256 a = bound(amt, 1, 1e5) * M;
        vm.prank(funding);
        ledger.distributeInterest(a, _ref());
        ghostFed += a;
    }

    function crossOut(uint8 b, uint8 h, uint64 amt) external {
        uint256 i = b % 3;
        address who = holders[h % 4];
        uint256 avail = tokens[i].availableBalanceOf(who);
        if (avail == 0) return;
        uint256 a = bound(amt, 1, avail);
        vm.recordLogs();
        vm.prank(who);
        home.depositForBurn(ids[i], a, 7, who);
        msgs.push(Msg(true, i, a, _lastMessage()));
        inFlight[i] += a;
    }

    function crossBack(uint8 b, uint8 h, uint64 amt) external {
        uint256 i = b % 3;
        address who = holders[h % 4];
        uint256 bal = remoteTokens[i].balanceOf(who);
        if (bal == 0) return;
        uint256 a = bound(amt, 1, bal);
        vm.recordLogs();
        vm.prank(who);
        remote.depositForBurn(ids[i], a, 0, who);
        msgs.push(Msg(false, i, a, _lastMessage()));
        inFlight[i] += a;
    }

    /// Sends to a wallet no bank admits on the other side: undeliverable,
    /// it can only come back by bounce.
    function crossOutStray(uint8 b, uint8 h, uint64 amt) external {
        uint256 i = b % 3;
        address who = holders[h % 4];
        uint256 avail = tokens[i].availableBalanceOf(who);
        if (avail == 0) return;
        uint256 a = bound(amt, 1, avail);
        vm.recordLogs();
        vm.prank(who);
        home.depositForBurn(ids[i], a, 7, address(0x5717A7));
        msgs.push(Msg(true, i, a, _lastMessage()));
        inFlight[i] += a;
    }

    /// Bounces a pending message if it is undeliverable; its RETURN replaces
    /// it in flight, same amount, the other way.
    function bounceOne(uint256 k) external {
        if (msgs.length == 0) return;
        uint256 idx = k % msgs.length;
        Msg memory m = msgs[idx];
        bytes[] memory sigs = _attest(m.body);
        bytes memory iss = _issuerSig(m.body, m.bank);
        vm.recordLogs();
        if (m.toRemote) {
            try remote.bounce(m.body, sigs, iss) { } catch { return; }
        } else {
            try home.bounce(m.body, sigs, iss) { } catch { return; }
        }
        msgs[idx] = Msg(!m.toRemote, m.bank, m.amount, _lastMessage());
        done["bounce"]++;
    }

    function deliver(uint256 k) external {
        if (msgs.length == 0) return;
        uint256 idx = k % msgs.length;
        Msg memory m = msgs[idx];
        msgs[idx] = msgs[msgs.length - 1];
        msgs.pop();
        bytes[] memory sigs = _attest(m.body);
        bytes memory iss = _issuerSig(m.body, m.bank);
        if (m.toRemote) remote.receiveMessage(m.body, sigs, iss);
        else home.receiveMessage(m.body, sigs, iss);
        inFlight[m.bank] -= m.amount;
        done[m.toRemote ? bytes32("deliverOut") : bytes32("deliverBack")]++;
    }

    /* helpers */

    function _i(bytes32 id) internal view returns (uint256) {
        return id == ids[0] ? 0 : id == ids[1] ? 1 : 2;
    }

    function _sort(bytes32[] memory a) internal pure {
        for (uint256 i = 1; i < a.length; i++) {
            bytes32 x = a[i];
            uint256 j = i;
            while (j > 0 && a[j - 1] > x) {
                a[j] = a[j - 1];
                j--;
            }
            a[j] = x;
        }
    }

    function _sortedMembers(int256[3] memory net) internal view returns (bytes32[] memory members, int256[] memory nets) {
        members = new bytes32[](3);
        nets = new int256[](3);
        for (uint256 i = 0; i < 3; i++) {
            (members[i], nets[i]) = (ids[i], net[i]);
        }
        for (uint256 i = 0; i < 3; i++) {
            for (uint256 j = i + 1; j < 3; j++) {
                if (members[j] < members[i]) {
                    (members[i], members[j]) = (members[j], members[i]);
                    (nets[i], nets[j]) = (nets[j], nets[i]);
                }
            }
        }
    }

    function _lastMessage() internal returns (bytes memory) {
        Vm.Log[] memory logs = vm.getRecordedLogs();
        for (uint256 i = logs.length; i > 0; i--) {
            if (logs[i - 1].topics[0] == keccak256("MessageSent(bytes)")) return abi.decode(logs[i - 1].data, (bytes));
        }
        revert("no message");
    }

    function _issuerSig(bytes memory body, uint256 bank) internal pure returns (bytes memory) {
        bytes32 digest = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", keccak256(body)));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(issuerKeyOf(bank), digest);
        return abi.encodePacked(r, s, v);
    }

    function _attest(bytes memory body) internal view returns (bytes[] memory sigs) {
        bytes32 digest = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", keccak256(body)));
        uint256 k0 = attesterKeys[0];
        uint256 k1 = attesterKeys[1];
        if (vm.addr(k1) < vm.addr(k0)) (k0, k1) = (k1, k0);
        sigs = new bytes[](2);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(k0, digest);
        sigs[0] = abi.encodePacked(r, s, v);
        (v, r, s) = vm.sign(k1, digest);
        sigs[1] = abi.encodePacked(r, s, v);
    }

    function inFlightOf(uint256 i) external view returns (uint256) {
        return inFlight[i];
    }

}

contract NetworkInvariantTest is StdInvariant, Test {

    uint256 constant M = 1e6;

    OmnibusLedger ledger;
    NetworkHandler handler;
    BankToken[3] tokens;
    RemoteBankToken[3] remoteTokens;
    bytes32[3] ids = [bytes32("BNKAUS30"), bytes32("BNKBUS30"), bytes32("BNKCUS30")];

    function setUp() public {
        vm.warp(1_800_000_000);
        address funding = makeAddr("funding");
        address so = makeAddr("settlement-op");
        ledger = new OmnibusLedger(address(this), 0);
        ledger.grantRole(ledger.GOVERNOR_ROLE(), address(this));
        ledger.grantRole(ledger.FUNDING_ROLE(), funding);
        PaymentRouter router = new PaymentRouter(ledger, address(this), 0);
        OmnibusNetting netting = new OmnibusNetting(ledger, address(this), 0);
        CrossChainMessenger home = new CrossChainMessenger(0, 0, ledger, address(this), 0);
        CrossChainMessenger remote = new CrossChainMessenger(7, 0, OmnibusLedger(address(0)), address(this), 0);
        ledger.grantRole(ledger.ROUTER_ROLE(), address(router));
        ledger.grantRole(ledger.NETTING_ROLE(), address(netting));
        ledger.grantRole(ledger.MESSENGER_ROLE(), address(home));
        netting.grantRole(netting.OPERATOR_ROLE(), so);

        uint256[3] memory ak = [uint256(0xA1), uint256(0xA2), uint256(0xA3)];
        for (uint256 i = 0; i < 3; i++) {
            home.setAttester(vm.addr(ak[i]), true);
            remote.setAttester(vm.addr(ak[i]), true);
        }
        home.setThreshold(2);
        remote.setThreshold(2);
        home.setRemote(7, address(remote));
        remote.setRemote(0, address(home));

        address[4] memory hs = [makeAddr("h0"), makeAddr("h1"), makeAddr("h2"), makeAddr("h3")];
        address[3][4] memory keys;
        string[3] memory names = ["bank-a", "bank-b", "bank-c"];
        for (uint256 i = 0; i < 3; i++) {
            keys[0][i] = makeAddr(string.concat("op-", names[i]));
            keys[1][i] = makeAddr(string.concat("app-", names[i]));
            keys[2][i] = makeAddr(string.concat("iss-", names[i]));
            keys[3][i] = makeAddr(string.concat("tre-", names[i]));

            HolderRegistry reg = new HolderRegistry(address(this), 0);
            reg.grantRole(reg.REGISTRAR_ROLE(), address(this));
            HolderRegistry regRemote = new HolderRegistry(address(this), 0);
            regRemote.grantRole(regRemote.REGISTRAR_ROLE(), address(this));
            for (uint256 h = 0; h < 4; h++) {
                reg.authorize(hs[h], "KYC");
                regRemote.authorize(hs[h], "KYC");
            }
            tokens[i] = new BankToken("Bank USD", "bUSD", ledger, address(router), reg, address(this), 0);
            tokens[i].grantRole(tokens[i].ISSUER_ROLE(), keys[2][i]);
            tokens[i].grantRole(tokens[i].MESSENGER_ROLE(), address(home));
            remoteTokens[i] = new RemoteBankToken("Bank USD", "bUSD", address(remote), regRemote, address(this), 0);
            ledger.admitMember(ids[i], address(tokens[i]), keys[0][i], keys[1][i], i == 1);
            ledger.registerWallet(ids[i], keys[3][i]);
            home.setToken(ids[i], address(tokens[i]));
            remote.setToken(ids[i], address(remoteTokens[i]));
            home.setCorridorCap(ids[i], 7, type(uint256).max);
            remote.setSupplyCap(ids[i], type(uint256).max);
            home.setIssuerAttester(ids[i], vm.addr(issuerKeyOf(i)));
            remote.setIssuerAttester(ids[i], vm.addr(issuerKeyOf(i)));
            vm.prank(funding);
            ledger.creditFunding(ids[i], 1_000_000 * M, keccak256(abi.encode("seed", i)));
        }
        ledger.setLimits(ids[2], 0, 50_000 * M); // Bank C keeps a prefund floor

        handler = new NetworkHandler(
            ledger, router, netting, home, remote, tokens, remoteTokens, ids, keys, hs, funding, so, ak, 3_000_000 * M
        );
        bytes4[] memory sel = new bytes4[](15);
        sel[0] = NetworkHandler.fund.selector;
        sel[1] = NetworkHandler.mint.selector;
        sel[2] = NetworkHandler.pay.selector;
        sel[3] = NetworkHandler.toTreasury.selector;
        sel[4] = NetworkHandler.settleHeld.selector;
        sel[5] = NetworkHandler.submit.selector;
        sel[6] = NetworkHandler.settleGross.selector;
        sel[7] = NetworkHandler.cycle.selector;
        sel[8] = NetworkHandler.defund.selector;
        sel[9] = NetworkHandler.interest.selector;
        sel[10] = NetworkHandler.crossOut.selector;
        sel[11] = NetworkHandler.crossBack.selector;
        sel[12] = NetworkHandler.deliver.selector;
        sel[13] = NetworkHandler.crossOutStray.selector;
        sel[14] = NetworkHandler.bounceOne.selector;
        targetSelector(FuzzSelector({ addr: address(handler), selectors: sel }));
        targetContract(address(handler));
    }

    function invariant_FedBalanceOnlyMovesWithFedwireAndInterest() public view {
        assertEq(ledger.omnibusTotal(), handler.ghostFed());
    }

    function invariant_BackingEqualsSupplyHereAndAbroad() public view {
        assertTrue(ledger.invariantsHold());
    }

    /// Recorded remote supply = tokens on the other chain + messages in flight.
    function invariant_RemoteSupplyIsExact() public view {
        for (uint256 i = 0; i < 3; i++) {
            assertEq(ledger.member(ids[i]).remoteSupply, remoteTokens[i].totalSupply() + handler.inFlightOf(i));
        }
    }

    /// With one other chain, that chain's corridor carries the whole remote
    /// supply: home's per-corridor count agrees with the ledger's total.
    function invariant_CorridorCountMatchesRemoteSupply() public view {
        for (uint256 i = 0; i < 3; i++) {
            assertEq(handler.home().outstanding(ids[i], 7), ledger.member(ids[i]).remoteSupply);
        }
    }

    /// No member's free position ever falls below zero or its prefund floor
    /// through minting or defunding; net debits come only from free position.
    function invariant_NoCreditAnywhere() public view {
        for (uint256 i = 0; i < 3; i++) {
            OmnibusLedger.Member memory m = ledger.member(ids[i]);
            assertLe(m.backing + m.pendingDefund, m.position);
        }
    }

}
