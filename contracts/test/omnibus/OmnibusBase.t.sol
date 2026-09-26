// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Test } from "forge-std/Test.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { PaymentRouter } from "src/omnibus/PaymentRouter.sol";

/// @dev Two member banks and their customers on one omnibus.
///      Bank A settles inbound payments instantly; Bank B accepts
///      each inbound payment explicitly, as an RTP receiver does.
abstract contract OmnibusBase is Test {

    uint256 constant M = 1e6; // $1

    bytes32 constant BANK_A = "BNKAUS30";
    bytes32 constant BANK_B = "BNKBUS30";

    OmnibusLedger ledger;
    PaymentRouter router;
    HolderRegistry aRegistry;
    HolderRegistry bRegistry;
    BankToken dtA;
    BankToken dtB;

    address governor = makeAddr("operator-governor");
    address funding = makeAddr("operator-funding-service");
    address reconciler = makeAddr("operator-reconciler");
    address aOperator = makeAddr("bank-a-gateway");
    address bOperator = makeAddr("bank-b-gateway");
    address aApprover = makeAddr("bank-a-treasury-approver");
    address bApprover = makeAddr("bank-b-treasury-approver");
    address aIssuer = makeAddr("bank-a-issuer");
    address bIssuer = makeAddr("bank-b-issuer");
    address aCompliance = makeAddr("bank-a-compliance");
    address bCompliance = makeAddr("bank-b-compliance");

    address alice = makeAddr("alice-at-bank-a");
    address carol = makeAddr("carol-at-bank-a");
    address bob = makeAddr("bob-at-bank-b");
    address outsider = makeAddr("outsider");

    uint256 t0 = 1_800_000_000;

    function setUp() public virtual {
        vm.warp(t0);
        ledger = new OmnibusLedger(address(this), 0);
        ledger.grantRole(ledger.GOVERNOR_ROLE(), governor);
        ledger.grantRole(ledger.FUNDING_ROLE(), funding);
        ledger.grantRole(ledger.RECONCILER_ROLE(), reconciler);

        router = new PaymentRouter(ledger, address(this), 0);
        ledger.grantRole(ledger.ROUTER_ROLE(), address(router));
        router.grantRole(router.PAUSER_ROLE(), address(this));

        (aRegistry, dtA) = _bank("Bank A USD", "A-dT", aIssuer, aCompliance);
        (bRegistry, dtB) = _bank("Bank B USD", "B-dT", bIssuer, bCompliance);

        vm.startPrank(governor);
        ledger.admitMember(BANK_A, address(dtA), aOperator, aApprover, false);
        ledger.admitMember(BANK_B, address(dtB), bOperator, bApprover, true);
        vm.stopPrank();

        aRegistry.authorize(alice, "KYC-A");
        aRegistry.authorize(carol, "KYC-C");
        bRegistry.authorize(bob, "KYC-B");
    }

    function _bank(string memory name, string memory symbol, address issuer, address compliance)
        internal
        returns (HolderRegistry reg, BankToken tok)
    {
        reg = new HolderRegistry(address(this), 0);
        reg.grantRole(reg.REGISTRAR_ROLE(), address(this));
        tok = new BankToken(name, symbol, ledger, address(router), reg, address(this), 0);
        tok.grantRole(tok.ISSUER_ROLE(), issuer);
        tok.grantRole(tok.COMPLIANCE_ROLE(), compliance);
    }

    function _fund(bytes32 memberId, uint256 amount, bytes32 fedRef) internal {
        vm.prank(funding);
        ledger.creditFunding(memberId, amount, fedRef);
    }

    function _mintA(address to, uint256 amount, bytes32 ref) internal {
        vm.prank(aIssuer);
        dtA.mint(to, amount, ref);
    }

    function _mintB(address to, uint256 amount, bytes32 ref) internal {
        vm.prank(bIssuer);
        dtB.mint(to, amount, ref);
    }

    function _position(bytes32 id) internal view returns (uint256) {
        return ledger.member(id).position;
    }

    function _backing(bytes32 id) internal view returns (uint256) {
        return ledger.member(id).backing;
    }

}
