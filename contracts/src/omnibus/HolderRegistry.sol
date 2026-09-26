// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { AccessControlDefaultAdminRules } from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";

/// @notice Who may hold one bank's ticker. Each issuing bank runs its own:
///         admitting a wallet is the bank's KYC decision about a customer,
///         a counterparty bank's settlement wallet, or a client of another
///         member it is willing to owe money to.
interface IHolderPolicy {

    function isAuthorized(address account) external view returns (bool);

}

/// @notice The one change another chain's messenger may make to a registry:
///         removing a holder the bank revoked at home.
interface IRevocable {

    function revoke(address account, bytes32 reason) external;

}

/// @title HolderRegistry — a bank's allowlist of wallets for its ticker
/// @dev The reference implementation the tests and the backend use. A
///      production bank points its ticker at whatever registry its
///      onboarding stack already maintains; the interface is one view call.
contract HolderRegistry is AccessControlDefaultAdminRules, IHolderPolicy, IRevocable {

    /// @notice The bank's onboarding service.
    bytes32 public constant REGISTRAR_ROLE = keccak256("REGISTRAR_ROLE");

    mapping(address account => bool) public authorized;

    event HolderAuthorized(address indexed account, bytes32 kycRef);
    event HolderRevoked(address indexed account, bytes32 reason);

    error ZeroAddress();

    constructor(address admin, uint48 adminDelay) AccessControlDefaultAdminRules(adminDelay, admin) { }

    function authorize(address account, bytes32 kycRef) external onlyRole(REGISTRAR_ROLE) {
        if (account == address(0)) revert ZeroAddress();
        authorized[account] = true;
        emit HolderAuthorized(account, kycRef);
    }

    function revoke(address account, bytes32 reason) external onlyRole(REGISTRAR_ROLE) {
        authorized[account] = false;
        emit HolderRevoked(account, reason);
    }

    function isAuthorized(address account) external view returns (bool) {
        return authorized[account];
    }

}
