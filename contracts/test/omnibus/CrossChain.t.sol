// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Vm } from "forge-std/Vm.sol";
import { OmnibusBase } from "./OmnibusBase.t.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { CrossChainMessenger } from "src/omnibus/crosschain/CrossChainMessenger.sol";
import { RemoteBankToken } from "src/omnibus/crosschain/RemoteBankToken.sol";
import { IERC7943FungibleToken } from "src/omnibus/IERC7943.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { TimelockController } from "@openzeppelin/contracts/governance/TimelockController.sol";
import { IAccessControl } from "@openzeppelin/contracts/access/IAccessControl.sol";

/// @dev Burn-and-mint with attestation, as CCTP moves USDC: home domain 0
///      (the omnibus chain) and a second chain, domain 7, simulated in the
///      same EVM. Three network attesters, two signatures required.
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

    function test_TokensLeaveByBurnAndArriveByMintWhileBackingStaysHome() public {
        bytes memory m = _out(200 * M);
        assertEq(dtA.balanceOf(alice), 300 * M, "burned at home");
        assertEq(ledger.member(BANK_A).remoteSupply, 200 * M);
        assertEq(_backing(BANK_A), 500 * M, "backing did not move");
        assertTrue(ledger.invariantsHold(), "backing == home supply + remote supply");

        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
        assertEq(dtARemote.balanceOf(alice), 200 * M, "minted on the other chain");

        // Home again: burn there, mint here.
        vm.recordLogs();
        vm.prank(alice);
        remote.depositForBurn(BANK_A, 150 * M, HOME, alice);
        bytes memory back = _lastMessage();
        home.receiveMessage(back, _attest(back, _two()), _iss(back));
        assertEq(dtA.balanceOf(alice), 450 * M);
        assertEq(dtARemote.balanceOf(alice), 50 * M);
        assertEq(ledger.member(BANK_A).remoteSupply, 50 * M);
        assertTrue(ledger.invariantsHold());
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

    function test_OnlyTheMessengerMintsOrBurns() public {
        vm.expectRevert(abi.encodeWithSelector(RemoteBankToken.OnlyMessenger.selector, address(this)));
        dtARemote.messengerMint(alice, 1);
        vm.prank(alice);
        vm.expectRevert();
        dtA.messengerMint(alice, 1);
    }

    /*//////////////////////////////////////////////////////////////////////////
                        Corridors, routing, delivery, pause
    //////////////////////////////////////////////////////////////////////////*/

    function _deliverOut(uint256 amount) internal {
        bytes memory m = _out(amount);
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
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

    /// A compromised attester set signs a message from the real remote
    /// messenger for more than was ever sent there: home refuses to mint it.
    function test_AChainCannotSendHomeMoreThanWasSentToIt() public {
        _deliverOut(100 * M);
        bytes memory forged = _forge(REMOTE, HOME, 0, address(remote), 101 * M);
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
        assertEq(ledger.member(BANK_A).remoteSupply, 50 * M, "still counted abroad: never under-backed");
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

        vm.prank(alice);
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

    function test_TCHsQuorumAloneCannotMintABanksDeposit() public {
        bytes memory m = _out(10 * M);
        bytes memory wrong = _signAs(attesterKeys[1], m); // an operator attester posing as the bank
        vm.expectRevert(
            abi.encodeWithSelector(CrossChainMessenger.BadIssuerAttestation.selector, BANK_A, attesters[1])
        );
        remote.receiveMessage(m, _attest(m, _two()), wrong);
    }

    function test_TheBankAloneCannotMintWithoutTCH() public {
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

    /// Tokens burned at home for a wallet that may not hold them on the other
    /// chain are not lost: anyone bounces the message and a RETURN re-mints
    /// them to the sender at home. The original can never mint afterwards.
    function test_AnUndeliverableTransferComesBackToItsSender() public {
        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurn(BANK_A, 50 * M, REMOTE, outsider);
        bytes memory m = _lastMessage();
        assertEq(dtA.balanceOf(alice), 450 * M);

        vm.recordLogs();
        remote.bounce(m, _attest(m, _two()), _iss(m));
        bytes memory ret = _lastMessage();

        aRegistryRemote.authorize(outsider, "LATE-KYC");
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NonceUsed.selector, HOME, uint64(0)));
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));

        home.receiveMessage(ret, _attest(ret, _two()), _iss(ret));
        assertEq(dtA.balanceOf(alice), 500 * M, "whole again");
        assertEq(home.outstanding(BANK_A, REMOTE), 0);
        assertEq(ledger.member(BANK_A).remoteSupply, 0);
        assertTrue(ledger.invariantsHold());
    }

    function test_ADeliverableTransferCannotBeBounced() public {
        bytes memory m = _out(10 * M);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.StillDeliverable.selector, alice));
        remote.bounce(m, _attest(m, _two()), _iss(m));
    }

    /// Home cannot deliver to an unadmitted wallet either; the RETURN goes back
    /// out, and if its sender has meanwhile lost admission too, the amount
    /// lands in the bank's suspense wallet rather than nowhere.
    function test_AReturnNobodyCanTakeLandsInTheBanksSuspenseWallet() public {
        _deliverOut(100 * M);
        vm.recordLogs();
        vm.prank(alice);
        remote.depositForBurn(BANK_A, 40 * M, HOME, outsider);
        bytes memory m = _lastMessage();

        vm.recordLogs();
        home.bounce(m, _attest(m, _two()), _iss(m));
        bytes memory ret = _lastMessage();
        assertEq(home.outstanding(BANK_A, REMOTE), 100 * M, "still counted abroad until the RETURN lands");

        aRegistryRemote.revoke(alice, "KYC-LAPSED");
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NoSuspense.selector, BANK_A));
        remote.receiveMessage(ret, _attest(ret, _two()), _iss(ret));

        address aSuspense = makeAddr("bank-a-suspense");
        aRegistryRemote.authorize(aSuspense, "SUSPENSE");
        remote.setSuspense(BANK_A, aSuspense);
        remote.receiveMessage(ret, _attest(ret, _two()), _iss(ret));
        assertEq(dtARemote.balanceOf(aSuspense), 40 * M);
        assertEq(dtARemote.totalSupply(), 100 * M, "every token abroad is accounted for");
        assertEq(ledger.member(BANK_A).remoteSupply, 100 * M);
        assertTrue(ledger.invariantsHold());
    }

    function test_AReturnIsNeverBounced() public {
        vm.recordLogs();
        vm.prank(alice);
        home.depositForBurn(BANK_A, 5 * M, REMOTE, outsider);
        bytes memory m = _lastMessage();
        vm.recordLogs();
        remote.bounce(m, _attest(m, _two()), _iss(m));
        bytes memory ret = _lastMessage();
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.NotBounceable.selector, uint8(1)));
        home.bounce(ret, _attest(ret, _two()), _iss(ret));
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

    /// A paused remote token makes delivery wait; it is not a reason to bounce.
    function test_APausedTokenMakesAMessageWaitNotBounce() public {
        bytes memory m = _out(10 * M);
        address aPauserRemote = makeAddr("bank-a-pauser-remote");
        dtARemote.grantRole(dtARemote.PAUSER_ROLE(), aPauserRemote);
        vm.prank(aPauserRemote);
        dtARemote.pause();
        vm.expectRevert(Pausable.EnforcedPause.selector);
        remote.receiveMessage(m, _attest(m, _two()), _iss(m));
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.StillDeliverable.selector, alice));
        remote.bounce(m, _attest(m, _two()), _iss(m));
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

}
