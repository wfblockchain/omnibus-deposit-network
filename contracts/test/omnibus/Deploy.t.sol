// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Test } from "forge-std/Test.sol";
import { TimelockController } from "@openzeppelin/contracts/governance/TimelockController.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { CrossChainMessenger } from "src/omnibus/crosschain/CrossChainMessenger.sol";
import { DvPSettlement } from "src/omnibus/dvp/DvPSettlement.sol";
import { DeployHome } from "script/DeployHome.s.sol";
import { DeployBank } from "script/DeployBank.s.sol";
import { DeployRemote } from "script/DeployRemote.s.sol";
import { VerifyRoles } from "script/VerifyRoles.s.sol";

/// @dev The deployment scripts, run end to end: every contract ends governed
///      by a timelock (or the bank's own governance) and the deployer ends
///      with no role anywhere.
contract DeployTest is Test {

    TimelockController timelock;
    address safe = makeAddr("operator-safe"); // proposes and executes on the timelock
    address bankSafe = makeAddr("bank-a-governance");
    VerifyRoles verify;

    function setUp() public {
        vm.warp(1_800_000_000);
        address[] memory who = new address[](1);
        who[0] = safe;
        timelock = new TimelockController(1 days, who, who, address(0));
        verify = new VerifyRoles();
    }

    function _attesters() internal returns (address[] memory a) {
        a = new address[](3);
        a[0] = makeAddr("att-1");
        a[1] = makeAddr("att-2");
        a[2] = makeAddr("att-3");
    }

    function _home(DeployHome h) internal returns (DeployHome.Deployed memory d) {
        d = h.deploy(
            DeployHome.Config({
                timelock: address(timelock),
                guardian: makeAddr("operator-guardian"),
                funding: makeAddr("operator-funding"),
                reconciler: makeAddr("operator-reconciler"),
                pauser: makeAddr("operator-pauser"),
                settlementOperator: makeAddr("operator-settlement"),
                attesters: _attesters(),
                threshold: 2,
                homeDomain: 0
            })
        );
    }

    /// Runs `calls` through the timelock: schedule, wait out the delay, execute.
    function _viaTimelock(address[] memory targets, bytes[] memory calls, bytes32 salt) internal {
        uint256[] memory values = new uint256[](targets.length);
        vm.prank(safe);
        timelock.scheduleBatch(targets, values, calls, bytes32(0), salt, 1 days);
        vm.prank(safe);
        vm.expectRevert(); // not before the delay
        timelock.executeBatch(targets, values, calls, bytes32(0), salt);
        vm.warp(block.timestamp + 1 days);
        vm.prank(safe);
        timelock.executeBatch(targets, values, calls, bytes32(0), salt);
    }

    function _acceptAll(address[] memory targets, bytes32 salt) internal {
        bytes[] memory calls = new bytes[](targets.length);
        for (uint256 i = 0; i < targets.length; i++) {
            calls[i] = abi.encodeWithSignature("acceptDefaultAdminTransfer()");
        }
        _viaTimelock(targets, calls, salt);
    }

    function test_TheHomeDeploymentClosesWithTheDeployerHoldingNothing() public {
        DeployHome h = new DeployHome();
        DeployHome.Deployed memory d = _home(h);
        address[] memory all = new address[](4);
        all[0] = address(d.ledger);
        all[1] = address(d.router);
        all[2] = address(d.netting);
        all[3] = address(d.messenger);

        assertGt(verify.check(all, address(h), address(timelock)), 0, "open until the timelock accepts");
        _acceptAll(all, "ACCEPT-HOME");
        assertEq(verify.check(all, address(h), address(timelock)), 0, "closed: deployer holds nothing");

        assertTrue(d.ledger.hasRole(d.ledger.GOVERNOR_ROLE(), address(timelock)), "admission is slow");
        assertTrue(d.ledger.hasRole(d.ledger.GUARDIAN_ROLE(), makeAddr("operator-guardian")), "suspension is fast");
        assertEq(d.messenger.threshold(), 2);
        assertEq(d.messenger.attesterCount(), 3);
    }

    /// A bank deploys its own ticker; the operator admits it only through the timelock.
    function test_ABankIsAdmittedOnlyThroughTheTimelock() public {
        DeployHome.Deployed memory d = _home(new DeployHome());
        DeployBank b = new DeployBank();
        (HolderRegistry reg, BankToken tok) = b.deployHome(
            DeployBank.Roles({
                governance: bankSafe,
                issuer: makeAddr("bank-a-issuer"),
                compliance: makeAddr("bank-a-compliance"),
                pauser: makeAddr("bank-a-pauser"),
                registrar: makeAddr("bank-a-registrar")
            }),
            "Bank A USD",
            "A-dT",
            d.ledger,
            address(d.router)
        );
        vm.warp(block.timestamp + 1); // OZ: acceptance only after the scheduled time has passed
        vm.prank(bankSafe);
        reg.acceptDefaultAdminTransfer();
        vm.prank(bankSafe);
        tok.acceptDefaultAdminTransfer();
        address[] memory bank = new address[](2);
        bank[0] = address(reg);
        bank[1] = address(tok);
        assertEq(verify.check(bank, address(b), bankSafe), 0, "the bank's deployer holds nothing");

        address[] memory t = new address[](1);
        t[0] = address(d.ledger);
        bytes[] memory calls = new bytes[](1);
        calls[0] = abi.encodeCall(
            OmnibusLedger.admitMember, ("BANKA", address(tok), makeAddr("bank-a-operator"), makeAddr("bank-a-approver"), false)
        );
        _viaTimelock(t, calls, "ADMIT-A");
        assertTrue(d.ledger.member("BANKA").admitted);
    }

    function test_TheRemoteDeploymentClosesToo() public {
        DeployHome.Deployed memory d = _home(new DeployHome());
        DeployRemote r = new DeployRemote();
        (CrossChainMessenger m, DvPSettlement venue) = r.deploy(
            DeployRemote.Config({
                timelock: address(timelock),
                guardian: makeAddr("operator-guardian"),
                attesters: _attesters(),
                threshold: 2,
                homeDomain: 0,
                localDomain: 7,
                homeMessenger: address(d.messenger),
                venueAdminDelay: 3 days
            })
        );
        address[] memory all = new address[](2);
        all[0] = address(m);
        all[1] = address(venue);
        address[] memory pending = new address[](1);
        pending[0] = address(m);
        _acceptAll(pending, "ACCEPT-REMOTE");
        assertEq(verify.check(all, address(r), address(timelock)), 0);
        assertEq(m.remoteMessenger(0), address(d.messenger));
        assertEq(venue.defaultAdminDelay(), 3 days);
    }

}
