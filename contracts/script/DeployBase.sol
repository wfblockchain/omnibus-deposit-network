// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Script } from "forge-std/Script.sol";
import { VmSafe } from "forge-std/Vm.sol";

/// @dev Shared by the deployment scripts: who is deploying, and the manifest.
abstract contract DeployBase is Script {

    /// The account that sends the deployment transactions: the broadcaster
    /// under `forge script --broadcast`, this contract when a test calls it.
    function _deployer() internal returns (address) {
        (VmSafe.CallerMode mode, address sender,) = vm.readCallers();
        if (mode == VmSafe.CallerMode.Broadcast || mode == VmSafe.CallerMode.RecurrentBroadcast) return sender;
        return address(this);
    }

    function _manifestPath(string memory name) internal view returns (string memory) {
        return string.concat(vm.projectRoot(), "/deployments/", vm.toString(block.chainid), "-", name, ".json");
    }

}
