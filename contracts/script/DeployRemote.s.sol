// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { DeployBase } from "./DeployBase.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { CrossChainMessenger } from "src/omnibus/crosschain/CrossChainMessenger.sol";
import { DvPSettlement } from "src/omnibus/dvp/DvPSettlement.sol";

/**
 * @title DeployRemote — the operator's contracts on another chain
 * @notice The spoke messenger and the DvP venue. Tokens, caps, issuer keys and
 *         suspense wallets are set afterwards through the timelock, bank by
 *         bank, as each is admitted to this chain.
 *
 *      Env: OPERATOR_TIMELOCK, OPERATOR_GUARDIAN, OPERATOR_ATTESTERS, OPERATOR_THRESHOLD,
 *      OPERATOR_HOME_DOMAIN, OPERATOR_LOCAL_DOMAIN, OPERATOR_HOME_MESSENGER, OPERATOR_ADMIN_DELAY.
 */
contract DeployRemote is DeployBase {

    struct Config {
        address timelock;
        address guardian;
        address[] attesters;
        uint8 threshold;
        uint32 homeDomain;
        uint32 localDomain;
        address homeMessenger;
        uint48 venueAdminDelay;
    }

    function run() external returns (CrossChainMessenger messenger, DvPSettlement venue) {
        Config memory c = Config({
            timelock: vm.envAddress("OPERATOR_TIMELOCK"),
            guardian: vm.envAddress("OPERATOR_GUARDIAN"),
            attesters: vm.envAddress("OPERATOR_ATTESTERS", ","),
            threshold: uint8(vm.envUint("OPERATOR_THRESHOLD")),
            homeDomain: uint32(vm.envUint("OPERATOR_HOME_DOMAIN")),
            localDomain: uint32(vm.envUint("OPERATOR_LOCAL_DOMAIN")),
            homeMessenger: vm.envAddress("OPERATOR_HOME_MESSENGER"),
            venueAdminDelay: uint48(vm.envUint("OPERATOR_ADMIN_DELAY"))
        });
        vm.startBroadcast();
        (messenger, venue) = deploy(c);
        vm.stopBroadcast();
        string memory k = "remote";
        vm.serializeAddress(k, "timelock", c.timelock);
        vm.serializeAddress(k, "messenger", address(messenger));
        string memory json = vm.serializeAddress(k, "dvp", address(venue));
        vm.writeJson(json, _manifestPath("remote"));
    }

    function deploy(Config memory c) public returns (CrossChainMessenger messenger, DvPSettlement venue) {
        address me = _deployer();
        messenger = new CrossChainMessenger(c.localDomain, c.homeDomain, OmnibusLedger(address(0)), me, 0);
        messenger.grantRole(messenger.PAUSER_ROLE(), c.guardian);
        for (uint256 i = 0; i < c.attesters.length; i++) {
            messenger.setAttester(c.attesters[i], true);
        }
        messenger.setThreshold(c.threshold);
        messenger.setRemote(c.homeDomain, c.homeMessenger);
        messenger.beginDefaultAdminTransfer(c.timelock);

        // The venue holds no admin power over funds; its admin (the timelock
        // from birth) can only grant the pauser role.
        venue = new DvPSettlement(messenger, c.timelock, c.venueAdminDelay);
    }

}
