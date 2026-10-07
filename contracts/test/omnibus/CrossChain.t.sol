// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Vm } from "forge-std/Vm.sol";
import { OmnibusBase } from "./OmnibusBase.t.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { CrossChainMessenger } from "src/omnibus/crosschain/CrossChainMessenger.sol";
import { RemoteBankToken } from "src/omnibus/crosschain/RemoteBankToken.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { IERC7943FungibleToken } from "src/omnibus/IERC7943.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { TimelockController } from "@openzeppelin/contracts/governance/TimelockController.sol";
import { IAccessControl } from "@openzeppelin/contracts/access/IAccessControl.sol";

/// @dev Lock, attested mint, then burn: home domain 0 (the omnibus chain) and
///      a second chain, domain 7, simulated in the same EVM. Three network
///      attesters, two signatures required, plus the issuing bank's key.
contract CrossChainTest is OmnibusBase {

    uint32 constant HOME = 0;
    uint32 constant REMOTE = 7;

    CrossChainMessenger home;
    CrossChainMessenger remote;
    RemoteBankToken dtARemote;
    HolderRegistry aRegistryRemote;

    uint256[3] attesterKeys = [uint256(0xA11CE), uint256(0xB0B), uint256(0xCA7)];
    uint256 constant BANK_A_ISSUER_KEY = 0x1550E; // Bank A's own attestation key
    address[3] attesters;

    function setUp() public override {
        super.setUp();
        home = new CrossChainMessenger(HOME, HOME, ledger, address(this), 0);
        remote = new CrossChainMessenger(REMOTE, HOME, OmnibusLedger(address(0)), address(this), 0);
        aRegistryRemote = new HolderRegistry(address(this), 0);
        aRegistryRemote.grantRole(aRegistryRemote.REGISTRAR_ROLE(), address(this));
        aRegistryRemote.authorize(alice, "KYC-A");
        dtARemote = new RemoteBankToken("Bank A USD", "A-dT", address(remote), aRegistryRemote, address(this), 0);

        ledger.grantRole(ledger.MESSENGER_ROLE(), address(home));
        dtA.grantRole(dtA.MESSENGER_ROLE(), address(home));

        for (uint256 i = 0; i < 3; i++) {
            attesters[i] = vm.addr(attesterKeys[i]);
            home.setAttester(attesters[i], true);
            remote.setAttester(attesters[i], true);
        }
        home.setThreshold(2);
        remote.setThreshold(2);
        home.setRemote(REMOTE, address(remote));
        remote.setRemote(HOME, address(home));
        home.setToken(BANK_A, address(dtA));
        remote.setToken(BANK_A, address(dtARemote));
        home.setCorridorCap(BANK_A, REMOTE, 400 * M);
        remote.setSupplyCap(BANK_A, 400 * M);
        home.setIssuerAttester(BANK_A, vm.addr(BANK_A_ISSUER_KEY));
        remote.setIssuerAttester(BANK_A, vm.addr(BANK_A_ISSUER_KEY));
        remote.setRateLimit(HOME, BANK_A, uint128(400 * M), 1 days);
        home.setRateLimit(REMOTE, BANK_A, uint128(400 * M), 1 days);

        _fund(BANK_A, 1_000 * M, "FW-BANK_A");
        _mintA(alice, 500 * M, "CORE-A-1");
    }

    function _lastMessage() internal returns (bytes memory) {
        Vm.Log[] memory logs = vm.getRecordedLogs();
        bytes32 sig = keccak256("MessageSent(bytes)");
        for (uint256 i = logs.length; i > 0; i--) {
            if (logs[i - 1].topics[0] == sig) return abi.decode(logs[i - 1].data, (bytes));
        }
        revert("no MessageSent");
    }

    /// Signatures from the given attesters, ordered by signer address.
    function _attest(bytes memory message, uint256[] memory which) internal view returns (bytes[] memory sigs) {
        bytes32 digest = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", keccak256(message)));
        uint256 n = which.length;
        address[] memory who = new address[](n);
        uint256[] memory keys = new uint256[](n);
        for (uint256 i = 0; i < n; i++) {
            keys[i] = attesterKeys[which[i]];
            who[i] = attesters[which[i]];
        }
        for (uint256 i = 0; i < n; i++) {
            for (uint256 j = i + 1; j < n; j++) {
                if (who[j] < who[i]) {
                    (who[i], who[j]) = (who[j], who[i]);
                    (keys[i], keys[j]) = (keys[j], keys[i]);
                }
            }
        }
        sigs = new bytes[](n);
        for (uint256 i = 0; i < n; i++) {
            (uint8 v, bytes32 r, bytes32 s) = vm.sign(keys[i], digest);
            sigs[i] = abi.encodePacked(r, s, v);
        }
    }

    /// The issuing bank's co-signature over the same message.
    function _iss(bytes memory message) internal pure returns (bytes memory) {
        return _signAs(BANK_A_ISSUER_KEY, message);
    }

    function _signAs(uint256 key, bytes memory message) internal pure returns (bytes memory) {
        bytes32 digest = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", keccak256(message)));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(key, digest);
        return abi.encodePacked(r, s, v);
    }

    /// A message as an attacker (or a compromised attester set) would forge it.
    function _forge(uint32 src, uint32 dst, uint64 nonce, address srcMessenger, uint256 amount)
        internal
        view
        returns (bytes memory)
    {
        return abi.encode(
            CrossChainMessenger.Envelope({
                version: remote.VERSION(),
                kind: 0,
                sourceDomain: src,
                destDomain: dst,
                nonce: nonce,
                sourceMessenger: srcMessenger,
                memberId: BANK_A,
                sender: alice,
                recipient: alice,
                amount: amount,
                destinationCaller: address(0),
                returnTo: alice,
                deadline: uint64(block.timestamp + 1 days),
                refNonce: 0,
                hookData: ""
            })
        );
    }

    /// A forged reply (MINT_ACK or MINT_CANCEL) to move `refNonce`.
    function _forgeReply(uint8 kind, uint32 src, uint32 dst, uint64 nonce, address srcMessenger, uint64 refNonce, uint256 amount)
        internal
        view
        returns (bytes memory)
    {
        return abi.encode(
            CrossChainMessenger.Envelope({
                version: remote.VERSION(),
                kind: kind,
                sourceDomain: src,
                destDomain: dst,
                nonce: nonce,
                sourceMessenger: srcMessenger,
                memberId: BANK_A,
                sender: alice,
                recipient: alice,
                amount: amount,
                destinationCaller: address(0),
                returnTo: alice,
                deadline: 0,
                refNonce: refNonce,
                hookData: ""
            })
        );
    }

    function _two() internal pure returns (uint256[] memory w) {
        w = new uint256[](2);
        w[0] = 0;
        w[1] = 2;
    }

    function _out(uint256 amount) internal returns (bytes memory message) {
        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurn(BANK_A, amount, REMOTE, alice);
        message = _lastMessage();
    }

    function _back(uint256 amount) internal returns (bytes memory message) {
        vm.recordLogs();
        vm.prank(alice);
        remote.depositForBurn(BANK_A, amount, HOME, alice);
        message = _lastMessage();
    }

    /// Delivers `m` on `on` and returns the reply it sends back.
    function _deliver(CrossChainMessenger on, bytes memory m) internal returns (bytes memory reply) {
        vm.recordLogs();
        on.receiveMessage(m, _attest(m, _two()), _iss(m));
        reply = _lastMessage();
    }

    function _cancel(CrossChainMessenger on, bytes memory m) internal returns (bytes memory reply) {
        vm.recordLogs();
        on.cancel(m, _attest(m, _two()), _iss(m));
        reply = _lastMessage();
    }

    function _settle(CrossChainMessenger on, bytes memory reply) internal {
        on.receiveMessage(reply, _attest(reply, _two()), _iss(reply));
    }

    function test_TokensLeaveByLockMintAndBurnWhileBackingStaysHome() public {
        bytes memory m = _out(200 * M);
        assertEq(dtA.balanceOf(alice), 300 * M, "locked at home");
        assertEq(dtA.balanceOf(address(home)), 200 * M, "in the messenger's escrow");
        assertEq(dtA.totalSupply(), 500 * M, "nothing burned yet");
        assertEq(ledger.member(BANK_A).remoteSupply, 0, "not abroad until minted there");
        assertEq(home.pendingOut(BANK_A, REMOTE), 200 * M);
        assertEq(_backing(BANK_A), 500 * M, "backing did not move");
        assertTrue(ledger.invariantsHold(), "backing == home supply + remote supply");

        bytes memory ack = _deliver(remote, m);
        assertEq(dtARemote.balanceOf(alice), 200 * M, "minted on the other chain");
        assertEq(uint8(remote.inbound(HOME, 0)), uint8(CrossChainMessenger.Inbound.Minted));

        _settle(home, ack);
        assertEq(dtA.balanceOf(address(home)), 0, "escrow burned on the acknowledgement");
        assertEq(dtA.totalSupply(), 300 * M);
        assertEq(ledger.member(BANK_A).remoteSupply, 200 * M);
        assertEq(home.outstanding(BANK_A, REMOTE), 200 * M);
        assertEq(home.pendingOut(BANK_A, REMOTE), 0);
        (CrossChainMessenger.MoveStatus st,,,,,) = home.moves(0);
        assertEq(uint8(st), uint8(CrossChainMessenger.MoveStatus.Completed));
        assertTrue(ledger.invariantsHold());

        // Home again: lock there, mint here, burn there.
        bytes memory back = _back(150 * M);
        bytes memory ack2 = _deliver(home, back);
        assertEq(dtA.balanceOf(alice), 450 * M);
        assertEq(ledger.member(BANK_A).remoteSupply, 50 * M, "counted off when minted home");
        assertEq(dtARemote.totalSupply(), 200 * M, "escrow abroad until the acknowledgement");
        assertTrue(ledger.invariantsHold());
        _settle(remote, ack2);
        assertEq(dtARemote.balanceOf(alice), 50 * M);
        assertEq(dtARemote.totalSupply(), 50 * M);
        assertEq(remote.escrowed(BANK_A), 0);
    }

    function test_BackingForTokensAbroadCannotBeDefunded() public {
        _out(200 * M);
        // BANK_A: position 1,000, backing 500 (300 here + 200 abroad): free 500.
        assertEq(ledger.freePosition(BANK_A), 500 * M);
        vm.prank(aOperator);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.InsufficientFreePosition.selector, BANK_A, 501 * M, 500 * M));
        ledger.requestDefund(BANK_A, 501 * M, "DEF-X");
    }

    function test_AMessageMintsOnce() public {
        bytes memory m = _out(10 * M);
        bytes[] memory sigs = _attest(m, _two());
        remote.receiveMessage(m, sigs, _iss(m));
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NonceUsed.selector, HOME, uint64(0)));
        remote.receiveMessage(m, sigs, _iss(m));
    }

    function test_OneAttesterIsNotEnough() public {
        bytes memory m = _out(10 * M);
        uint256[] memory one = new uint256[](1);
        one[0] = 1;
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NotEnoughAttestations.selector, 1, 2));
        remote.receiveMessage(m, _attest(m, one), _iss(m));
    }

    function test_AForgedSourceMessengerIsRefused() public {
        bytes memory forged = _forge(HOME, REMOTE, 99, address(0xBAD), 1_000_000 * M);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.UntrustedSource.selector, HOME, address(0xBAD)));
        remote.receiveMessage(forged, _attest(forged, _two()), _iss(forged));
    }

    function test_UnsortedOrRepeatedSignaturesAreRefused() public {
        bytes memory m = _out(10 * M);
        bytes[] memory sigs = _attest(m, _two());
        bytes[] memory swapped = new bytes[](2);
        swapped[0] = sigs[1];
        swapped[1] = sigs[0];
        vm.expectRevert(CrossChainMessenger.SignersNotSorted.selector);
        remote.receiveMessage(m, swapped, _iss(m));

        bytes[] memory twice = new bytes[](2);
        twice[0] = sigs[0];
        twice[1] = sigs[0];
        vm.expectRevert(CrossChainMessenger.SignersNotSorted.selector);
        remote.receiveMessage(m, twice, _iss(m));
    }

    function test_OnlyTheMessengerMintsLocksOrBurns() public {
        vm.expectRevert(abi.encodeWithSelector(RemoteBankToken.OnlyMessenger.selector, address(this)));
        dtARemote.messengerMint(alice, 1);
        vm.expectRevert(abi.encodeWithSelector(RemoteBankToken.OnlyMessenger.selector, address(this)));
        dtARemote.messengerLock(alice, 1);
        vm.expectRevert(abi.encodeWithSelector(RemoteBankToken.OnlyMessenger.selector, address(this)));
        dtARemote.messengerBurnLocked(1);
        vm.expectRevert(abi.encodeWithSelector(RemoteBankToken.OnlyMessenger.selector, address(this)));
        dtARemote.messengerUnlock(alice, 1);
        vm.startPrank(alice);
        vm.expectRevert();
        dtA.messengerMint(alice, 1);
        vm.expectRevert();
        dtA.messengerLock(alice, 1);
        vm.expectRevert();
        dtA.messengerBurnLocked(1);
        vm.expectRevert();
        dtA.messengerUnlock(alice, 1);
        vm.stopPrank();
    }

    /*//////////////////////////////////////////////////////////////////////////
                        Corridors, routing, delivery, pause
    //////////////////////////////////////////////////////////////////////////*/

    /// A whole move out: lock, mint there, acknowledgement home.
    function _deliverOut(uint256 amount) internal {
        _settle(home, _deliver(remote, _out(amount)));
    }

    function test_ACorridorIsClosedUntilGovernanceOpensIt() public {
        home.setCorridorCap(BANK_A, REMOTE, 0);
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.CorridorCapExceeded.selector, BANK_A, REMOTE, 1, 0));
        home.depositForBurn(BANK_A, 1, REMOTE, alice);
    }

    function test_TheCorridorCapBoundsWhatSitsOnTheOtherChain() public {
        _deliverOut(300 * M);
        vm.prank(alice);
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.CorridorCapExceeded.selector, BANK_A, REMOTE, 401 * M, 400 * M)
        );
        home.depositForBurn(BANK_A, 101 * M, REMOTE, alice);
        assertEq(home.outstanding(BANK_A, REMOTE), 300 * M);
    }

    /// Moves still in escrow reserve the corridor too, so the cap holds
    /// whichever of them are minted.
    function test_PendingMovesReserveTheCorridorCap() public {
        _out(300 * M);
        vm.prank(alice);
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.CorridorCapExceeded.selector, BANK_A, REMOTE, 401 * M, 400 * M)
        );
        home.depositForBurn(BANK_A, 101 * M, REMOTE, alice);
    }

    /// A compromised attester set signs a message from the real remote
    /// messenger for more than was ever sent there: home refuses to mint it.
    function test_AChainCannotSendHomeMoreThanWasSentToIt() public {
        _deliverOut(100 * M);
        bytes memory forged = _forge(REMOTE, HOME, 50, address(remote), 101 * M);
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.CorridorUnderflow.selector, BANK_A, REMOTE, 101 * M, 100 * M)
        );
        home.receiveMessage(forged, _attest(forged, _two()), _iss(forged));
    }

    function test_OtherChainsTalkOnlyToHome() public {
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.SpokeToSpoke.selector, uint32(9)));
        remote.setRemote(9, address(0xBEEF));
    }

    function test_OnlyTheNamedCallerDeliversAMessageWithInstructions() public {
        address venue = makeAddr("venue");
        aRegistryRemote.authorize(venue, "VENUE");
        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurnWithHook(BANK_A, 10 * M, REMOTE, venue, venue, abi.encode(bytes32("TRADE-1")), address(0));
        bytes memory m = _lastMessage();
        bytes[] memory sigs = _attest(m, _two());

        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.WrongCaller.selector, address(this), venue));
        remote.receiveMessage(m, sigs, _iss(m));

        vm.prank(venue);
        CrossChainMessenger.Delivery memory d = remote.receiveMessage(m, sigs, _iss(m));
        assertEq(d.recipient, venue);
        assertEq(d.sender, alice);
        assertEq(d.token, address(dtARemote));
        assertEq(d.amount, 10 * M);
        assertEq(abi.decode(d.hookData, (bytes32)), bytes32("TRADE-1"));
        assertEq(dtARemote.balanceOf(venue), 10 * M);
    }

    function test_APauseStopsSendingAndDeliveryButLosesNothing() public {
        bytes memory m = _out(50 * M);
        remote.grantRole(remote.PAUSER_ROLE(), address(this));
        remote.pause();
        bytes[] memory sigs = _attest(m, _two());
        vm.expectRevert(Pausable.EnforcedPause.selector);
        remote.receiveMessage(m, sigs, _iss(m));
        assertEq(dtA.balanceOf(address(home)), 50 * M, "still in escrow at home: never under-backed");
        assertTrue(ledger.invariantsHold());
        remote.unpause();
        remote.receiveMessage(m, sigs, _iss(m));
        assertEq(dtARemote.balanceOf(alice), 50 * M);
    }

    function test_AMessageToAnUnadmittedWalletCannotBeDelivered() public {
        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurn(BANK_A, 5 * M, REMOTE, outsider);
        bytes memory m = _lastMessage();
        vm.expectRevert(abi.encodeWithSelector(IERC7943FungibleToken.ERC7943CannotReceive.selector, outsider));
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
    }

    /*//////////////////////////////////////////////////////////////////////////
                    The remote token keeps the bank's controls
    //////////////////////////////////////////////////////////////////////////*/

    function _remoteCompliance() internal returns (address c) {
        c = makeAddr("bank-a-compliance-remote");
        dtARemote.grantRole(dtARemote.COMPLIANCE_ROLE(), c);
    }

    function test_RemoteFreezeBindsTransfersAndBurnsHome() public {
        _deliverOut(100 * M);
        address c = _remoteCompliance();
        vm.prank(c);
        dtARemote.setFrozenTokens(alice, 80 * M);
        aRegistryRemote.authorize(carol, "KYC-C");

        vm.prank(alice);
        vm.expectRevert(
            abi.encodeWithSelector(IERC7943FungibleToken.ERC7943InsufficientUnfrozenBalance.selector, alice, 30 * M, 20 * M)
        );
        dtARemote.transfer(carol, 30 * M);

        vm.prank(alice); // the lock respects the freeze too
        vm.expectRevert(
            abi.encodeWithSelector(IERC7943FungibleToken.ERC7943InsufficientUnfrozenBalance.selector, alice, 30 * M, 20 * M)
        );
        remote.depositForBurn(BANK_A, 30 * M, HOME, alice);
        assertFalse(dtARemote.canTransfer(alice, carol, 30 * M));
        assertTrue(dtARemote.canTransfer(alice, carol, 20 * M));
    }

    function test_RemoteForcedTransferAndRecovery() public {
        _deliverOut(100 * M);
        address c = _remoteCompliance();
        aRegistryRemote.authorize(carol, "KYC-C");
        vm.prank(c);
        dtARemote.setFrozenTokens(alice, 100 * M);
        vm.prank(c);
        dtARemote.forcedTransfer(alice, carol, 40 * M);
        assertEq(dtARemote.balanceOf(carol), 40 * M);
        assertEq(dtARemote.getFrozenTokens(alice), 60 * M, "freeze shrinks to what is left");

        address fresh = makeAddr("alice-new-key");
        aRegistryRemote.authorize(fresh, "KYC-A2");
        vm.prank(c);
        dtARemote.recover(alice, fresh);
        assertEq(dtARemote.balanceOf(fresh), 60 * M);
        assertTrue(dtARemote.blocked(alice));
        assertFalse(dtARemote.canReceive(alice), "the lost key never receives again");
        assertTrue(dtARemote.supportsInterface(type(IERC7943FungibleToken).interfaceId));
    }

    /*//////////////////////////////////////////////////////////////////////////
                    Two keys, capped on both sides, slow to change
    //////////////////////////////////////////////////////////////////////////*/

    function test_TheOperatorsQuorumAloneCannotMintABanksDeposit() public {
        bytes memory m = _out(10 * M);
        bytes memory wrong = _signAs(attesterKeys[1], m); // an operator attester posing as the bank
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.BadIssuerAttestation.selector, BANK_A, attesters[1])
        );
        remote.receiveMessage(m, _attest(m, _two()), wrong);
    }

    function test_TheBankAloneCannotMintWithoutTheOperator() public {
        bytes memory m = _out(10 * M);
        uint256[] memory one = new uint256[](1);
        one[0] = 0;
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NotEnoughAttestations.selector, 1, 2));
        remote.receiveMessage(m, _attest(m, one), _iss(m));
    }

    function test_AKeyNeverHoldsBothRoles() public {
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.KeyHasOtherRole.selector, attesters[0]));
        remote.setIssuerAttester(BANK_B, attesters[0]);
        address aKey = vm.addr(BANK_A_ISSUER_KEY);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.KeyHasOtherRole.selector, aKey));
        remote.setAttester(aKey, true);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.KeyHasOtherRole.selector, aKey));
        remote.setIssuerAttester(BANK_B, aKey);
    }

    /// Worst case: the operator's attesters AND the bank's key are both compromised and
    /// sign a mint that no burn at home ever backed. The chain's supply cap
    /// is the ceiling on the damage.
    function test_EvenBothKeySetsStopAtTheSupplyCap() public {
        bytes memory forged = _forge(HOME, REMOTE, 999, address(home), 401 * M);
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.SupplyCapExceeded.selector, BANK_A, 401 * M, 400 * M)
        );
        remote.receiveMessage(forged, _attest(forged, _two()), _iss(forged));
    }

    function test_ARotatedIssuerKeyStopsSigningAtOnce() public {
        bytes memory m = _out(10 * M);
        uint256 fresh = 0xF2E5;
        remote.setIssuerAttester(BANK_A, vm.addr(fresh));
        vm.expectRevert(
            abi.encodeWithSelector(
                CrossChainMessenger.BadIssuerAttestation.selector, BANK_A, vm.addr(BANK_A_ISSUER_KEY)
            )
        );
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
        remote.receiveMessage(m, _attest(m, _two()), _signAs(fresh, m));
        assertEq(dtARemote.balanceOf(alice), 10 * M);
        assertEq(remote.issuerOfKey(vm.addr(BANK_A_ISSUER_KEY)), bytes32(0), "the old key is gone");
    }

    /// In production the admin is a TimelockController: a new attester is
    /// public for the whole delay before it can sign anything.
    function test_ConfigurationWaitsOutTheTimelock() public {
        address[] memory who = new address[](1);
        who[0] = address(this);
        TimelockController tl = new TimelockController(2 days, who, who, address(0));
        CrossChainMessenger m = new CrossChainMessenger(9, HOME, OmnibusLedger(address(0)), address(tl), 3 days);
        address rogue = makeAddr("rogue-attester");

        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector, address(this), m.DEFAULT_ADMIN_ROLE()
            )
        );
        m.setAttester(rogue, true);

        bytes memory call = abi.encodeCall(CrossChainMessenger.setAttester, (rogue, true));
        tl.schedule(address(m), 0, call, bytes32(0), "ATT-1", 2 days);
        vm.expectRevert();
        tl.execute(address(m), 0, call, bytes32(0), "ATT-1");
        vm.warp(block.timestamp + 2 days);
        tl.execute(address(m), 0, call, bytes32(0), "ATT-1");
        assertTrue(m.isAttester(rogue));
        assertEq(m.defaultAdminDelay(), 3 days, "handing over the admin is slow too");
    }

    /*//////////////////////////////////////////////////////////////////////////
                Undeliverable messages, thresholds, guardian caps
    //////////////////////////////////////////////////////////////////////////*/

    /// Tokens locked at home for a wallet that may not hold them on the other
    /// chain are not lost: anyone cancels the move there at once, and the
    /// MINT_CANCEL returns the escrow at home. The original can never mint.
    function test_AnUndeliverableTransferIsCancelledAndItsEscrowReturns() public {
        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurn(BANK_A, 50 * M, REMOTE, outsider);
        bytes memory m = _lastMessage();
        assertEq(dtA.balanceOf(alice), 450 * M);

        bytes memory c = _cancel(remote, m);
        assertEq(uint8(remote.inbound(HOME, 0)), uint8(CrossChainMessenger.Inbound.Cancelled));

        aRegistryRemote.authorize(outsider, "LATE-KYC");
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NonceUsed.selector, HOME, uint64(0)));
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));

        _settle(home, c);
        assertEq(dtA.balanceOf(alice), 500 * M, "whole again");
        assertEq(dtA.balanceOf(address(home)), 0);
        assertEq(home.pendingOut(BANK_A, REMOTE), 0);
        assertEq(home.outstanding(BANK_A, REMOTE), 0);
        assertEq(ledger.member(BANK_A).remoteSupply, 0);
        assertTrue(ledger.invariantsHold());
    }

    function test_ADeliverableTransferCannotBeCancelledBeforeItsDeadline() public {
        bytes memory m = _out(10 * M);
        (,, uint64 deadline,,,) = home.moves(0);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NotYetCancellable.selector, deadline, alice));
        remote.cancel(m, _attest(m, _two()), _iss(m));
    }

    /// Home cannot deliver to an unadmitted wallet either; the move is
    /// cancelled, and if its sender has meanwhile lost admission too, the
    /// escrow lands in the bank's suspense wallet rather than nowhere.
    function test_CancelledEscrowNobodyCanTakeLandsInTheBanksSuspenseWallet() public {
        _deliverOut(100 * M);
        vm.recordLogs();
        vm.prank(alice);
        remote.depositForBurn(BANK_A, 40 * M, HOME, outsider);
        bytes memory m = _lastMessage();

        bytes memory c = _cancel(home, m);
        assertEq(home.outstanding(BANK_A, REMOTE), 100 * M, "never counted off: never minted home");

        aRegistryRemote.revoke(alice, "KYC-LAPSED");
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NoSuspense.selector, BANK_A));
        remote.receiveMessage(c, _attest(c, _two()), _iss(c));

        address aSuspense = makeAddr("bank-a-suspense");
        aRegistryRemote.authorize(aSuspense, "SUSPENSE");
        remote.setSuspense(BANK_A, aSuspense);
        _settle(remote, c);
        assertEq(dtARemote.balanceOf(aSuspense), 40 * M);
        assertEq(dtARemote.totalSupply(), 100 * M, "every token abroad is accounted for");
        assertEq(ledger.member(BANK_A).remoteSupply, 100 * M);
        assertTrue(ledger.invariantsHold());
    }

    function test_OnlyATransferCanBeCancelled() public {
        bytes memory ack = _deliver(remote, _out(5 * M));
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NotCancellable.selector, uint8(3)));
        home.cancel(ack, _attest(ack, _two()), _iss(ack));
    }

    /// Three attesters, threshold two. The threshold must stay a strict
    /// majority and reachable: lower it before removing, raise it before adding.
    function test_TheThresholdStaysAStrictAndReachableMajority() public {
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.BadThreshold.selector, 1, 3));
        remote.setThreshold(1);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.BadThreshold.selector, 4, 3));
        remote.setThreshold(4);

        address extra = makeAddr("attester-4");
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.BadThreshold.selector, 2, 4));
        remote.setAttester(extra, true); // 2 of 4 is not a majority
        remote.setThreshold(3);
        remote.setAttester(extra, true);
        assertEq(remote.attesterCount(), 4);

        remote.setAttester(extra, false);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.BadThreshold.selector, 3, 2));
        remote.setAttester(attesters[0], false); // 3 of 2 is unreachable
    }

    function test_TheGuardianLowersCapsAtOnceButCannotRaiseThem() public {
        address guardian = makeAddr("operator-guardian");
        home.grantRole(home.PAUSER_ROLE(), guardian);
        vm.prank(guardian);
        home.lowerCorridorCap(BANK_A, REMOTE, 100 * M);
        assertEq(home.corridorCap(BANK_A, REMOTE), 100 * M);

        vm.prank(guardian);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NotALowering.selector, 100 * M, 500 * M));
        home.lowerCorridorCap(BANK_A, REMOTE, 500 * M);

        bytes32 adminRole = home.DEFAULT_ADMIN_ROLE();
        vm.prank(guardian);
        vm.expectRevert(
            abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, guardian, adminRole)
        );
        home.setCorridorCap(BANK_A, REMOTE, 500 * M);
    }

    /// A paused remote token makes delivery wait; before the deadline it is
    /// not a reason to cancel.
    function test_APausedTokenMakesAMessageWaitNotCancel() public {
        bytes memory m = _out(10 * M);
        address aPauserRemote = makeAddr("bank-a-pauser-remote");
        dtARemote.grantRole(dtARemote.PAUSER_ROLE(), aPauserRemote);
        vm.prank(aPauserRemote);
        dtARemote.pause();
        vm.expectRevert(Pausable.EnforcedPause.selector);
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
        (,, uint64 deadline,,,) = home.moves(0);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NotYetCancellable.selector, deadline, alice));
        remote.cancel(m, _attest(m, _two()), _iss(m));
        vm.prank(aPauserRemote);
        dtARemote.unpause();
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
        assertEq(dtARemote.balanceOf(alice), 10 * M);
    }

    /*//////////////////////////////////////////////////////////////////////////
                        Revocations follow the token
    //////////////////////////////////////////////////////////////////////////*/

    function test_ARevocationAtHomeReachesTheOtherChain() public {
        _deliverOut(100 * M);
        aRegistryRemote.grantRole(aRegistryRemote.REGISTRAR_ROLE(), address(remote));
        aRegistryRemote.authorize(carol, "KYC-C");

        vm.recordLogs();
        home.sendRevocation(BANK_A, alice, REMOTE, "OFAC-HIT"); // this test is BANK_A's registrar at home
        bytes memory m = _lastMessage();
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));

        assertFalse(aRegistryRemote.authorized(alice), "revoked on the other chain too");
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(IERC7943FungibleToken.ERC7943CannotSend.selector, alice));
        dtARemote.transfer(carol, 1 * M);
    }

    function test_OnlyTheBanksRegistrarBroadcastsARevocation() public {
        vm.prank(outsider);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NotRegistrar.selector, BANK_A, outsider));
        home.sendRevocation(BANK_A, alice, REMOTE, "X");
    }

    function test_OnlyHomeBroadcastsRevocations() public {
        vm.expectRevert(CrossChainMessenger.HomeOnly.selector);
        remote.sendRevocation(BANK_A, alice, HOME, "X");
    }

    /// Three attesters, threshold two. The guardian drops a suspect key at
    /// once; dropping a second would leave two-of-one, so it is refused.
    function test_TheGuardianRemovesASuspectAttesterAtOnce() public {
        address guardian = makeAddr("operator-guardian");
        remote.grantRole(remote.PAUSER_ROLE(), guardian);
        vm.prank(guardian);
        remote.disableAttester(attesters[1]);
        assertFalse(remote.isAttester(attesters[1]));
        assertEq(remote.attesterCount(), 2);

        vm.prank(guardian);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.BadThreshold.selector, 2, 1));
        remote.disableAttester(attesters[0]);

        bytes memory m = _out(10 * M); // attesters 0 and 2 still deliver
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
        assertEq(dtARemote.balanceOf(alice), 10 * M);
    }

    /*//////////////////////////////////////////////////////////////////////////
                    Inbound rate limit, on the receiving chain
    //////////////////////////////////////////////////////////////////////////*/

    /// A chain connected with everything but a bucket mints nothing: the
    /// rate limit is closed until governance opens it.
    function test_AnUnsetBucketIsClosed() public {
        uint32 other = 8;
        CrossChainMessenger m8 = new CrossChainMessenger(other, HOME, OmnibusLedger(address(0)), address(this), 0);
        RemoteBankToken t8 = new RemoteBankToken("Bank A USD", "A-dT", address(m8), aRegistryRemote, address(this), 0);
        for (uint256 i = 0; i < 3; i++) {
            m8.setAttester(attesters[i], true);
        }
        m8.setThreshold(2);
        m8.setRemote(HOME, address(home));
        m8.setToken(BANK_A, address(t8));
        m8.setSupplyCap(BANK_A, 400 * M);
        m8.setIssuerAttester(BANK_A, vm.addr(BANK_A_ISSUER_KEY));
        home.setRemote(other, address(m8));
        home.setCorridorCap(BANK_A, other, 400 * M);

        (uint256 cap, uint64 window, uint256 avail) = m8.rateLimit(HOME, BANK_A);
        assertEq(cap + window + avail, 0, "unset");

        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurn(BANK_A, 10 * M, other, alice);
        bytes memory m = _lastMessage();
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.RateLimited.selector, HOME, BANK_A, 10 * M, 0));
        m8.receiveMessage(m, _attest(m, _two()), _iss(m));

        m8.setRateLimit(HOME, BANK_A, uint128(100 * M), 1 days);
        m8.receiveMessage(m, _attest(m, _two()), _iss(m));
        assertEq(t8.balanceOf(alice), 10 * M, "the same message, once governance opens the bucket");
    }

    /// A message over the bucket reverts and stays deliverable; it goes
    /// through once the bucket has refilled, linearly over the window.
    function test_TheBucketRefillsLinearlyAndAnOverLimitMintWaits() public {
        remote.setRateLimit(HOME, BANK_A, uint128(100 * M), 1 days);
        _deliver(remote, _out(100 * M));
        (,, uint256 avail) = remote.rateLimit(HOME, BANK_A);
        assertEq(avail, 0, "drained");

        bytes memory m = _out(30 * M);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.RateLimited.selector, HOME, BANK_A, 30 * M, 0));
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));

        // Absolute times from the fixture's t0 (storage): under via-IR a local
        // copy of block.timestamp can be re-read after a warp.
        vm.warp(t0 + 6 hours);
        (,, avail) = remote.rateLimit(HOME, BANK_A);
        assertEq(avail, 25 * M, "a quarter of the window, a quarter of the capacity");
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.RateLimited.selector, HOME, BANK_A, 30 * M, 25 * M));
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));

        vm.warp(t0 + 12 hours);
        _deliver(remote, m); // retried: the message was never lost
        (,, avail) = remote.rateLimit(HOME, BANK_A);
        assertEq(avail, 20 * M);
        assertEq(dtARemote.balanceOf(alice), 130 * M);

        vm.warp(t0 + 10 days);
        (,, avail) = remote.rateLimit(HOME, BANK_A);
        assertEq(avail, 100 * M, "refills to the capacity, never past it");
    }

    function test_TheGuardianOnlyTightensTheBucket() public {
        address guardian = makeAddr("operator-guardian");
        remote.grantRole(remote.PAUSER_ROLE(), guardian);
        _deliver(remote, _out(100 * M)); // 300 of 400 left

        vm.prank(guardian);
        remote.lowerRateLimit(HOME, BANK_A, uint128(50 * M), 1 days);
        (uint256 cap, uint64 window, uint256 avail) = remote.rateLimit(HOME, BANK_A);
        assertEq(cap, 50 * M);
        assertEq(window, 1 days);
        assertEq(avail, 50 * M, "the level never exceeds the new capacity");

        vm.prank(guardian);
        remote.lowerRateLimit(HOME, BANK_A, uint128(50 * M), 2 days); // a slower refill
        vm.startPrank(guardian);
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.RateLimitNotLowered.selector, 50 * M, 2 days, 60 * M, 2 days)
        );
        remote.lowerRateLimit(HOME, BANK_A, uint128(60 * M), 2 days);
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.RateLimitNotLowered.selector, 50 * M, 2 days, 40 * M, 1 days)
        );
        remote.lowerRateLimit(HOME, BANK_A, uint128(40 * M), 1 days); // a faster refill is a loosening
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.RateLimitNotLowered.selector, 50 * M, 2 days, 50 * M, 2 days)
        );
        remote.lowerRateLimit(HOME, BANK_A, uint128(50 * M), 2 days);
        bytes32 adminRole = remote.DEFAULT_ADMIN_ROLE();
        vm.expectRevert(
            abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, guardian, adminRole)
        );
        remote.setRateLimit(HOME, BANK_A, uint128(500 * M), 1 days);

        remote.lowerRateLimit(HOME, BANK_A, 0, 2 days); // closed at once
        vm.stopPrank();
        bytes memory m = _out(1);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.RateLimited.selector, HOME, BANK_A, 1, 0));
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
    }

    function test_ABucketNeedsAWindow() public {
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.BadRateLimit.selector, 1, 0));
        remote.setRateLimit(HOME, BANK_A, 1, 0);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.UnknownDomain.selector, REMOTE));
        remote.setRateLimit(REMOTE, BANK_A, 1, 1 days);
    }

    /// The worst case again: the operator's attesters AND the bank's key sign
    /// messages no lock at home ever backed (a verifier fed by poisoned
    /// nodes would do the same). Every check on the message passes, and the
    /// receiving chain's bucket is what still bounds the damage per window,
    /// well under the supply cap.
    function test_AForgedMessageWithBothKeysIsStillRateLimited() public {
        _deliverOut(300 * M);
        remote.setRateLimit(HOME, BANK_A, uint128(50 * M), 1 days);
        bytes memory f1 = _forge(HOME, REMOTE, 900, address(home), 50 * M);
        remote.receiveMessage(f1, _attest(f1, _two()), _iss(f1));
        assertEq(dtARemote.balanceOf(alice), 350 * M, "every signature on the forgery was valid");

        bytes memory f2 = _forge(HOME, REMOTE, 901, address(home), 1 * M);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.RateLimited.selector, HOME, BANK_A, 1 * M, 0));
        remote.receiveMessage(f2, _attest(f2, _two()), _iss(f2));
        assertLt(dtARemote.totalSupply(), remote.supplyCap(BANK_A));

        // Home is bounded the same way on what comes back.
        home.setRateLimit(REMOTE, BANK_A, uint128(20 * M), 1 days);
        bytes memory f3 = _forge(REMOTE, HOME, 902, address(remote), 21 * M);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.RateLimited.selector, REMOTE, BANK_A, 21 * M, 20 * M));
        home.receiveMessage(f3, _attest(f3, _two()), _iss(f3));
    }

    /// Escrow released by a MINT_CANCEL is value appearing on this chain too,
    /// so it draws on the same bucket.
    function test_ACancelReleasingEscrowDrawsOnTheBucket() public {
        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurn(BANK_A, 50 * M, REMOTE, outsider);
        bytes memory c = _cancel(remote, _lastMessage());
        home.setRateLimit(REMOTE, BANK_A, uint128(10 * M), 1 days);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.RateLimited.selector, REMOTE, BANK_A, 50 * M, 10 * M));
        home.receiveMessage(c, _attest(c, _two()), _iss(c));
        home.setRateLimit(REMOTE, BANK_A, uint128(50 * M), 1 days);
        vm.warp(block.timestamp + 1 days);
        _settle(home, c);
        assertEq(dtA.balanceOf(alice), 500 * M);
    }

    /*//////////////////////////////////////////////////////////////////////////
                    Lock then burn: nothing burned before its mint
    //////////////////////////////////////////////////////////////////////////*/

    function test_NothingBurnsBeforeTheMintIsAcknowledged() public {
        bytes memory m = _out(80 * M);
        bytes memory ack = _deliver(remote, m);
        assertEq(dtARemote.balanceOf(alice), 80 * M);
        assertEq(dtA.totalSupply(), 500 * M, "minted there, still escrowed here");
        assertEq(home.escrowed(BANK_A), 80 * M);
        assertEq(home.outstanding(BANK_A, REMOTE), 0);
        assertTrue(ledger.invariantsHold());

        CrossChainMessenger.Envelope memory a = abi.decode(ack, (CrossChainMessenger.Envelope));
        assertEq(a.kind, remote.KIND_MINT_ACK());
        assertEq(a.refNonce, 0);
        assertEq(a.amount, 80 * M);

        _settle(home, ack);
        assertEq(dtA.totalSupply(), 420 * M);
        assertEq(home.escrowed(BANK_A), 0);
        assertEq(home.outstanding(BANK_A, REMOTE), 80 * M);
    }

    /// The destination refuses: here its issuer stops co-signing, so no mint
    /// can happen. Past the deadline nobody can mint the move any more, and
    /// anyone cancels it; the escrow comes back.
    function test_ARefusedMoveIsCancelledAfterItsDeadlineAndTheEscrowReturns() public {
        bytes memory m = _out(60 * M);
        remote.setIssuerAttester(BANK_A, vm.addr(0xDEAD)); // the bank stops signing here
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.BadIssuerAttestation.selector, BANK_A, vm.addr(BANK_A_ISSUER_KEY))
        );
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
        remote.setIssuerAttester(BANK_A, vm.addr(BANK_A_ISSUER_KEY));

        (,, uint64 deadline,,,) = home.moves(0);
        vm.warp(deadline);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.Expired.selector, HOME, uint64(0), deadline));
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));

        remote.grantRole(remote.PAUSER_ROLE(), address(this));
        remote.pause(); // a halted messenger still lets senders recover
        bytes memory c = _cancel(remote, m);
        _settle(home, c);
        assertEq(dtA.balanceOf(alice), 500 * M);
        assertEq(home.escrowed(BANK_A), 0);
        assertEq(home.pendingOut(BANK_A, REMOTE), 0);
        (CrossChainMessenger.MoveStatus st,,,,,) = home.moves(0);
        assertEq(uint8(st), uint8(CrossChainMessenger.MoveStatus.Cancelled));
        assertTrue(ledger.invariantsHold());
    }

    function test_ANonceIsMintedOrCancelledNeverBoth() public {
        bytes memory m1 = _out(10 * M);
        _deliver(remote, m1);
        vm.warp(t0 + 2 days);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NonceUsed.selector, HOME, uint64(0)));
        remote.cancel(m1, _attest(m1, _two()), _iss(m1));

        bytes memory m2 = _out(10 * M);
        vm.warp(t0 + 4 days);
        _cancel(remote, m2);
        uint64 n2 = abi.decode(m2, (CrossChainMessenger.Envelope)).nonce;
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NonceUsed.selector, HOME, n2));
        remote.receiveMessage(m2, _attest(m2, _two()), _iss(m2));
        assertEq(dtARemote.totalSupply(), 10 * M, "minted once");
    }

    /// Replies apply once, only to a pending move, and only as sent: a replay,
    /// a reply to a settled move, or one that misstates the move is refused.
    function test_RepliesAreNotReplayable() public {
        bytes memory ack = _deliver(remote, _out(10 * M));
        _settle(home, ack);
        uint64 ackNonce = abi.decode(ack, (CrossChainMessenger.Envelope)).nonce;
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NonceUsed.selector, REMOTE, ackNonce));
        home.receiveMessage(ack, _attest(ack, _two()), _iss(ack));

        // Even with both keys, a cancel for the completed move is refused...
        bytes memory c = _forgeReply(4, REMOTE, HOME, 777, address(remote), 0, 10 * M);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.UnknownMove.selector, REMOTE, uint64(0)));
        home.receiveMessage(c, _attest(c, _two()), _iss(c));

        // ...and so is an acknowledgement that misstates a pending one.
        _out(20 * M);
        bytes memory bad = _forgeReply(3, REMOTE, HOME, 778, address(remote), 1, 21 * M);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.UnknownMove.selector, REMOTE, uint64(1)));
        home.receiveMessage(bad, _attest(bad, _two()), _iss(bad));
        assertEq(home.escrowed(BANK_A), 20 * M);
    }

    function test_TheMoveTimeoutIsBounded() public {
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.BadMoveTimeout.selector, uint64(1)));
        home.setMoveTimeout(1);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.BadMoveTimeout.selector, uint64(31 days)));
        home.setMoveTimeout(31 days);
        home.setMoveTimeout(2 hours);
        _out(1 * M);
        (,, uint64 deadline,,,) = home.moves(0);
        assertEq(deadline, block.timestamp + 2 hours);
    }

    /// The escrow is the messenger's balance, but the messenger is never a
    /// holder: nobody transfers to it, and compliance cannot move or freeze it.
    function test_TheEscrowIsNotAHolder() public {
        aRegistry.authorize(address(home), "MISTAKE");
        aRegistryRemote.authorize(address(remote), "MISTAKE");
        _out(10 * M);
        _deliverOut(20 * M);
        _back(5 * M);

        assertFalse(dtA.canReceive(address(home)));
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(IERC7943FungibleToken.ERC7943CannotReceive.selector, address(home)));
        dtA.transfer(address(home), 1);
        vm.startPrank(aCompliance);
        vm.expectRevert(abi.encodeWithSelector(BankToken.EscrowAccount.selector, address(home)));
        dtA.forcedTransfer(address(home), carol, 1);
        vm.expectRevert(abi.encodeWithSelector(BankToken.EscrowAccount.selector, address(home)));
        dtA.setFrozenTokens(address(home), 1);
        vm.expectRevert(abi.encodeWithSelector(BankToken.EscrowAccount.selector, address(home)));
        dtA.recover(address(home), carol);
        vm.stopPrank();

        address c = _remoteCompliance();
        assertFalse(dtARemote.canReceive(address(remote)));
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(IERC7943FungibleToken.ERC7943CannotReceive.selector, address(remote)));
        dtARemote.transfer(address(remote), 1);
        vm.startPrank(c);
        vm.expectRevert(abi.encodeWithSelector(RemoteBankToken.EscrowAccount.selector, address(remote)));
        dtARemote.forcedTransfer(address(remote), alice, 1);
        vm.expectRevert(abi.encodeWithSelector(RemoteBankToken.EscrowAccount.selector, address(remote)));
        dtARemote.setFrozenTokens(address(remote), 1);
        vm.expectRevert(abi.encodeWithSelector(RemoteBankToken.EscrowAccount.selector, address(remote)));
        dtARemote.recover(address(remote), alice);
        vm.stopPrank();

        // A transfer addressed to the messenger itself can only be cancelled.
        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurn(BANK_A, 1 * M, REMOTE, address(remote));
        bytes memory m = _lastMessage();
        _settle(home, _cancel(remote, m));
        assertEq(home.escrowed(BANK_A), 10 * M);
        assertEq(dtA.balanceOf(address(home)), 10 * M);
    }

}
