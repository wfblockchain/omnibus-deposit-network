// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { DeployBase } from "./DeployBase.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { RemoteBankToken } from "src/omnibus/crosschain/RemoteBankToken.sol";

/**
 * @title DeployBank — one member bank's registry and ticker
 * @notice Run by the bank, under its own deployer key, on the home chain
 *         (`run`) or on another chain (`runRemote`). Roles go to the bank's
 *         own issuer, compliance, pauser and registrar; admin begins handing
 *         over to the bank's governance (its timelock or Safe). Admission to
 *         the network is the operator's decision, made later through the operator's timelock.
 *         The governance accepts the admin role (acceptDefaultAdminTransfer)
 *         in a later block: OpenZeppelin requires the scheduled time to have
 *         passed, even with a zero delay.
 *
 *      Env: BANK_GOVERNANCE, BANK_ISSUER, BANK_COMPLIANCE, BANK_PAUSER,
 *      BANK_REGISTRAR, BANK_NAME, BANK_TICKER, and for home OPERATOR_LEDGER,
 *      OPERATOR_ROUTER; for another chain OPERATOR_MESSENGER.
 */
contract DeployBank is DeployBase {

    struct Roles {
        address governance;
        address issuer;
        address compliance;
        address pauser;
        address registrar;
    }

    function _roles() internal view returns (Roles memory) {
        return Roles({
            governance: vm.envAddress("BANK_GOVERNANCE"),
            issuer: vm.envAddress("BANK_ISSUER"),
            compliance: vm.envAddress("BANK_COMPLIANCE"),
            pauser: vm.envAddress("BANK_PAUSER"),
            registrar: vm.envAddress("BANK_REGISTRAR")
        });
    }

    function run() external returns (HolderRegistry reg, BankToken tok) {
        Roles memory r = _roles();
        vm.startBroadcast();
        (reg, tok) = deployHome(
            r, vm.envString("BANK_NAME"), vm.envString("BANK_TICKER"), OmnibusLedger(vm.envAddress("OPERATOR_LEDGER")), vm.envAddress("OPERATOR_ROUTER")
        );
        vm.stopBroadcast();
    }

    function runRemote() external returns (HolderRegistry reg, RemoteBankToken tok) {
        Roles memory r = _roles();
        vm.startBroadcast();
        (reg, tok) = deployRemote(r, vm.envString("BANK_NAME"), vm.envString("BANK_TICKER"), vm.envAddress("OPERATOR_MESSENGER"));
        vm.stopBroadcast();
    }

    function deployHome(Roles memory r, string memory name, string memory ticker, OmnibusLedger ledger, address router)
        public
        returns (HolderRegistry reg, BankToken tok)
    {
        address me = _deployer();
        reg = new HolderRegistry(me, 0);
        tok = new BankToken(name, ticker, ledger, router, reg, me, 0);
        reg.grantRole(reg.REGISTRAR_ROLE(), r.registrar);
        tok.grantRole(tok.ISSUER_ROLE(), r.issuer);
        tok.grantRole(tok.COMPLIANCE_ROLE(), r.compliance);
        tok.grantRole(tok.PAUSER_ROLE(), r.pauser);
        reg.beginDefaultAdminTransfer(r.governance);
        tok.beginDefaultAdminTransfer(r.governance);
    }

    /// @dev The remote registry lets the messenger apply revocations the bank
    ///      broadcasts from home; admitting holders stays with the registrar.
    function deployRemote(Roles memory r, string memory name, string memory ticker, address messenger)
        public
        returns (HolderRegistry reg, RemoteBankToken tok)
    {
        address me = _deployer();
        reg = new HolderRegistry(me, 0);
        tok = new RemoteBankToken(name, ticker, messenger, reg, me, 0);
        reg.grantRole(reg.REGISTRAR_ROLE(), r.registrar);
        reg.grantRole(reg.REGISTRAR_ROLE(), messenger);
        tok.grantRole(tok.COMPLIANCE_ROLE(), r.compliance);
        tok.grantRole(tok.PAUSER_ROLE(), r.pauser);
        reg.beginDefaultAdminTransfer(r.governance);
        tok.beginDefaultAdminTransfer(r.governance);
    }

}
