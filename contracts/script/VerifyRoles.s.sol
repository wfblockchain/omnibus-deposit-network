// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Script, console2 } from "forge-std/Script.sol";
import { IAccessControl } from "@openzeppelin/contracts/access/IAccessControl.sol";
import { IAccessControlDefaultAdminRules } from
    "@openzeppelin/contracts/access/extensions/IAccessControlDefaultAdminRules.sol";

/**
 * @title VerifyRoles — the deployer must end with nothing
 * @notice Fails unless, on every contract, the admin is the expected
 *         governance and the deployer holds no role at all. Deployer keys left
 *         holding admin rights are how Stake DAO (Jun 2026) and Wasabi
 *         (Apr 2026) were drained; this is the check that closes a deployment.
 *
 *      Env: VERIFY_CONTRACTS (comma-separated), VERIFY_DEPLOYER,
 *      VERIFY_ADMIN (the timelock or bank governance).
 */
contract VerifyRoles is Script {

    bytes32[] internal ROLES;

    constructor() {
        ROLES.push(0x00); // DEFAULT_ADMIN_ROLE
        string[16] memory names = [
            "GOVERNOR_ROLE", "GUARDIAN_ROLE", "FUNDING_ROLE", "RECONCILER_ROLE", "ROUTER_ROLE", "NETTING_ROLE",
            "MESSENGER_ROLE", "PAUSER_ROLE", "OPERATOR_ROLE", "ISSUER_ROLE", "COMPLIANCE_ROLE", "REGISTRAR_ROLE",
            "PROPOSER_ROLE", "EXECUTOR_ROLE", "CANCELLER_ROLE", "TIMELOCK_ADMIN_ROLE"
        ];
        for (uint256 i = 0; i < names.length; i++) {
            ROLES.push(keccak256(bytes(names[i])));
        }
    }

    function run() external view {
        uint256 bad = check(vm.envAddress("VERIFY_CONTRACTS", ","), vm.envAddress("VERIFY_DEPLOYER"), vm.envAddress("VERIFY_ADMIN"));
        require(bad == 0, "VerifyRoles: deployment not closed");
    }

    /// @return problems how many checks failed (each is logged).
    function check(address[] memory contracts, address deployer, address admin) public view returns (uint256 problems) {
        for (uint256 i = 0; i < contracts.length; i++) {
            address c = contracts[i];
            for (uint256 j = 0; j < ROLES.length; j++) {
                if (IAccessControl(c).hasRole(ROLES[j], deployer)) {
                    console2.log("deployer still holds a role on", c);
                    console2.logBytes32(ROLES[j]);
                    problems++;
                }
            }
            if (IAccessControlDefaultAdminRules(c).defaultAdmin() != admin) {
                console2.log("admin is not governance on", c);
                problems++;
            }
            (address pending,) = IAccessControlDefaultAdminRules(c).pendingDefaultAdmin();
            if (pending != address(0)) {
                console2.log("an admin handover is still pending on", c);
                problems++;
            }
        }
    }

}
