// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { DeployBase } from "./DeployBase.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { PaymentRouter } from "src/omnibus/PaymentRouter.sol";
import { OmnibusNetting } from "src/omnibus/OmnibusNetting.sol";
import { CrossChainMessenger } from "src/omnibus/crosschain/CrossChainMessenger.sol";

/**
 * @title DeployHome — the operator's contracts on the omnibus chain
 * @notice Deploys the ledger, router, netting and home messenger, gives each
 *         operational role to its holder, and begins handing every contract's
 *         admin to the timelock. The deployer holds only the admin role, and
 *         only until the timelock accepts it (see VerifyRoles).
 *
 * @dev Restrictive roles (guardian, pauser) go to fast holders; the governor
 *      role, which admits members and changes their keys, goes to the
 *      timelock itself. Admin delay starts at zero so the timelock can accept
 *      at once; the timelock then raises it (changeDefaultAdminDelay).
 *
 *      Env: OPERATOR_TIMELOCK, OPERATOR_GUARDIAN, OPERATOR_FUNDING, OPERATOR_RECONCILER,
 *      OPERATOR_PAUSER, OPERATOR_SETTLEMENT_OPERATOR, OPERATOR_ATTESTERS (comma-separated),
 *      OPERATOR_THRESHOLD, OPERATOR_HOME_DOMAIN.
 */
contract DeployHome is DeployBase {

    struct Config {
        address timelock;
        address guardian;
        address funding;
        address reconciler;
        address pauser;
        address settlementOperator;
        address[] attesters;
        uint8 threshold;
        uint32 homeDomain;
    }

    struct Deployed {
        OmnibusLedger ledger;
        PaymentRouter router;
        OmnibusNetting netting;
        CrossChainMessenger messenger;
    }

    function run() external returns (Deployed memory d) {
        Config memory c = Config({
            timelock: vm.envAddress("OPERATOR_TIMELOCK"),
            guardian: vm.envAddress("OPERATOR_GUARDIAN"),
            funding: vm.envAddress("OPERATOR_FUNDING"),
            reconciler: vm.envAddress("OPERATOR_RECONCILER"),
            pauser: vm.envAddress("OPERATOR_PAUSER"),
            settlementOperator: vm.envAddress("OPERATOR_SETTLEMENT_OPERATOR"),
            attesters: vm.envAddress("OPERATOR_ATTESTERS", ","),
            threshold: uint8(vm.envUint("OPERATOR_THRESHOLD")),
            homeDomain: uint32(vm.envUint("OPERATOR_HOME_DOMAIN"))
        });
        vm.startBroadcast();
        d = deploy(c);
        vm.stopBroadcast();

        string memory k = "home";
        vm.serializeAddress(k, "timelock", c.timelock);
        vm.serializeAddress(k, "ledger", address(d.ledger));
        vm.serializeAddress(k, "router", address(d.router));
        vm.serializeAddress(k, "netting", address(d.netting));
        string memory json = vm.serializeAddress(k, "messenger", address(d.messenger));
        vm.writeJson(json, _manifestPath("home"));
    }

    function deploy(Config memory c) public returns (Deployed memory d) {
        address me = _deployer();
        d.ledger = new OmnibusLedger(me, 0);
        d.router = new PaymentRouter(d.ledger, me, 0);
        d.netting = new OmnibusNetting(d.ledger, me, 0);
        d.messenger = new CrossChainMessenger(c.homeDomain, c.homeDomain, d.ledger, me, 0);

        OmnibusLedger l = d.ledger;
        l.grantRole(l.GOVERNOR_ROLE(), c.timelock);
        l.grantRole(l.GUARDIAN_ROLE(), c.guardian);
        l.grantRole(l.FUNDING_ROLE(), c.funding);
        l.grantRole(l.RECONCILER_ROLE(), c.reconciler);
        l.grantRole(l.ROUTER_ROLE(), address(d.router));
        l.grantRole(l.NETTING_ROLE(), address(d.netting));
        l.grantRole(l.MESSENGER_ROLE(), address(d.messenger));
        d.router.grantRole(d.router.PAUSER_ROLE(), c.pauser);
        d.netting.grantRole(d.netting.OPERATOR_ROLE(), c.settlementOperator);
        d.netting.grantRole(d.netting.PAUSER_ROLE(), c.pauser);
        d.messenger.grantRole(d.messenger.PAUSER_ROLE(), c.guardian);
        for (uint256 i = 0; i < c.attesters.length; i++) {
            d.messenger.setAttester(c.attesters[i], true);
        }
        d.messenger.setThreshold(c.threshold);

        l.beginDefaultAdminTransfer(c.timelock);
        d.router.beginDefaultAdminTransfer(c.timelock);
        d.netting.beginDefaultAdminTransfer(c.timelock);
        d.messenger.beginDefaultAdminTransfer(c.timelock);
    }

}
