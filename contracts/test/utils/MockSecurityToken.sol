// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { ERC20 } from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import { IERC165 } from "@openzeppelin/contracts/utils/introspection/IERC165.sol";
import { IERC7943FungibleToken } from "src/omnibus/IERC7943.sol";

/// @dev A tokenized fund share the way a transfer agent issues one on a
///      public chain: an allowlist on both sides of every transfer, partial
///      freezes, forced transfers, and the ERC-7943 surface. 18 decimals, so
///      tests cover legs with different decimals from the 6-decimal cash.
contract MockSecurityToken is ERC20, IERC7943FungibleToken {

    address public agent;
    mapping(address => bool) public eligible;
    mapping(address => uint256) private _frozen;

    constructor() ERC20("Tokenized T-Bill Fund", "TBILL") {
        agent = msg.sender;
    }

    function setAgent(address a) external {
        require(msg.sender == agent, "agent");
        agent = a;
    }

    function setEligible(address a, bool ok) external {
        require(msg.sender == agent, "agent");
        eligible[a] = ok;
    }

    function issue(address to, uint256 amount) external {
        require(msg.sender == agent, "agent");
        _mint(to, amount);
    }

    function canSend(address a) public view returns (bool) {
        return eligible[a];
    }

    function canReceive(address a) public view returns (bool) {
        return eligible[a];
    }

    function canTransfer(address from, address to, uint256 amount) external view returns (bool) {
        return canSend(from) && canReceive(to) && balanceOf(from) - _frozen[from] >= amount;
    }

    function getFrozenTokens(address a) external view returns (uint256) {
        return _frozen[a];
    }

    function setFrozenTokens(address a, uint256 amount) external returns (bool) {
        require(msg.sender == agent, "agent");
        _frozen[a] = amount;
        emit Frozen(a, amount);
        return true;
    }

    function forcedTransfer(address from, address to, uint256 amount) external returns (bool) {
        require(msg.sender == agent, "agent");
        super._update(from, to, amount);
        emit ForcedTransfer(from, to, amount);
        return true;
    }

    function supportsInterface(bytes4 id) external pure returns (bool) {
        return id == type(IERC7943FungibleToken).interfaceId || id == type(IERC165).interfaceId;
    }

    function _update(address from, address to, uint256 value) internal override {
        if (from != address(0)) {
            if (!canSend(from)) revert ERC7943CannotSend(from);
            uint256 free = balanceOf(from) - _frozen[from];
            if (value > free) revert ERC7943InsufficientUnfrozenBalance(from, value, free);
        }
        if (to != address(0) && !canReceive(to)) revert ERC7943CannotReceive(to);
        super._update(from, to, value);
    }

}

/// @dev Takes 1% of every transfer: a leg in this token never arrives whole.
contract MockFeeToken is ERC20 {

    constructor() ERC20("Fee Share", "FEE") {
        _mint(msg.sender, 1e30);
    }

    function _update(address from, address to, uint256 value) internal override {
        if (from != address(0) && to != address(0)) {
            uint256 fee = value / 100;
            super._update(from, address(0xFEE), fee);
            value -= fee;
        }
        super._update(from, to, value);
    }

}

/// @dev Calls back into a target during every transfer, as an ERC-777-style
///      hook would: the attack a reentrancy guard exists for.
contract MockReentrantToken is ERC20 {

    address public target;
    bytes public payload;
    bool public reentered;
    bool public reentrySucceeded;

    constructor() ERC20("Hook Share", "HOOK") {
        _mint(msg.sender, 1e30);
    }

    function arm(address t, bytes calldata p) external {
        target = t;
        payload = p;
    }

    function _update(address from, address to, uint256 value) internal override {
        super._update(from, to, value);
        if (target != address(0) && !reentered) {
            reentered = true;
            (reentrySucceeded,) = target.call(payload);
        }
    }

}
