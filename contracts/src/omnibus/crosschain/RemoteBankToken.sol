// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { ERC20 } from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { AccessControlDefaultAdminRules } from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";
import { IERC165 } from "@openzeppelin/contracts/utils/introspection/IERC165.sol";
import { IERC7943FungibleToken } from "../IERC7943.sol";
import { IHolderPolicy } from "../HolderRegistry.sol";
import { IBurnMintToken } from "./IBurnMintToken.sol";

/**
 * @title RemoteBankToken — a member's tokenized deposit on another chain
 * @notice Natively issued on the remote chain, but only by the messenger, on
 *         an attested move from home; burned only when leaving, once its
 *         mint at home is proven. Its reserves
 *         stay in the operator's joint account and are counted on the home ledger as
 *         the member's remote supply.
 *
 * @dev THE SAME CONTROLS AS AT HOME. A deposit is the bank's liability on
 *      every chain, so the bank keeps on a public chain what it has at home:
 *      its holder policy on both sides of every transfer, partial freezes,
 *      regulatory forced transfers, and key recovery that blocks the lost key
 *      for good. The ERC-7943 surface is the same one BankToken reports, so a
 *      settlement venue can pre-check a transfer with `canTransfer` on either
 *      chain.
 *
 *      WHAT IS DIFFERENT. No router, no holds and no ledger here: cross-ticker
 *      conversion happens only at home. A settlement contract that must hold
 *      the token (a DvP escrow, say) is a holder like any other and needs the
 *      bank's admission to its policy.
 */
contract RemoteBankToken is ERC20, AccessControlDefaultAdminRules, Pausable, IERC7943FungibleToken, IBurnMintToken {

    /// @notice The issuing bank's compliance function on this chain.
    bytes32 public constant COMPLIANCE_ROLE = keccak256("COMPLIANCE_ROLE");

    /// @notice The issuing bank's emergency stop on this chain: no transfer,
    ///         mint, burn, forced transfer or recovery while paused; freezing
    ///         still works.
    ///         Messages for a paused token wait; they are cancelled only once
    ///         their deadline passes.
    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");

    address public immutable MESSENGER;

    IHolderPolicy public policy;

    mapping(address account => uint256) private _frozen;
    mapping(address account => bool) public blocked;

    event PolicyChanged(address indexed policy);
    event KeyRecovered(address indexed lost, address indexed replacement, uint256 amount);

    error OnlyMessenger(address caller);
    error ZeroAddress();
    error SelfTransfer();
    error EscrowAccount(address account);

    /// @param admin the issuing bank's governance; it can replace the holder
    ///        policy, so its handover is two-step and delayed.
    constructor(
        string memory name_,
        string memory symbol_,
        address messenger,
        IHolderPolicy policy_,
        address admin,
        uint48 adminDelay
    ) ERC20(name_, symbol_) AccessControlDefaultAdminRules(adminDelay, admin) {
        if (messenger == address(0) || address(policy_) == address(0)) revert ZeroAddress();
        MESSENGER = messenger;
        policy = policy_;
    }

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Messenger legs
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Escrows a holder's tokens leaving for home. The messenger
    ///         calls it only for the sender that called it. Locks respect
    ///         freezes. The escrow is the messenger's balance, reachable only
    ///         through these legs.
    function messengerLock(address from, uint256 amount) external {
        _onlyMessenger();
        _requireNotPaused();
        if (!canSend(from)) revert ERC7943CannotSend(from);
        uint256 u = unfrozenBalanceOf(from);
        if (amount > u) revert ERC7943InsufficientUnfrozenBalance(from, amount, u);
        super._update(from, MESSENGER, amount);
    }

    /// @notice Burns escrow whose mint at home is proven.
    function messengerBurnLocked(uint256 amount) external {
        _onlyMessenger();
        _requireNotPaused();
        super._update(MESSENGER, address(0), amount);
    }

    /// @notice Returns escrow of a cancelled move to an admitted holder.
    function messengerUnlock(address to, uint256 amount) external {
        _onlyMessenger();
        _requireNotPaused();
        if (!canReceive(to)) revert ERC7943CannotReceive(to);
        super._update(MESSENGER, to, amount);
    }

    function _onlyMessenger() internal view {
        if (msg.sender != MESSENGER) revert OnlyMessenger(msg.sender);
    }

    /// @notice Mints on an attested burn at home. The receiver must be
    ///         admitted here, or the message cannot be delivered.
    function messengerMint(address to, uint256 amount) external {
        _onlyMessenger();
        _mint(to, amount);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                ERC-7943 surface
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The bank's admitted holders here; never the messenger, whose
    ///         escrow is not a holding.
    function canSend(address account) public view returns (bool) {
        return !blocked[account] && account != MESSENGER && policy.isAuthorized(account);
    }

    function canReceive(address account) public view returns (bool) {
        return !blocked[account] && account != MESSENGER && policy.isAuthorized(account);
    }

    function canTransfer(address from, address to, uint256 amount) external view returns (bool) {
        return !paused() && canSend(from) && canReceive(to) && unfrozenBalanceOf(from) >= amount;
    }

    function getFrozenTokens(address account) external view returns (uint256) {
        return _frozen[account];
    }

    function unfrozenBalanceOf(address account) public view returns (uint256) {
        uint256 bal = balanceOf(account);
        uint256 frz = _frozen[account];
        return bal > frz ? bal - frz : 0;
    }

    function setFrozenTokens(address account, uint256 amount) external onlyRole(COMPLIANCE_ROLE) returns (bool) {
        if (account == address(0)) revert ZeroAddress();
        if (account == MESSENGER) revert EscrowAccount(account);
        _frozen[account] = amount;
        emit Frozen(account, amount);
        return true;
    }

    /// @notice Regulatory transfer between two real accounts; overrides
    ///         freezes and the sender's policy, never the receiver's.
    function forcedTransfer(address from, address to, uint256 amount)
        external
        onlyRole(COMPLIANCE_ROLE)
        whenNotPaused
        returns (bool)
    {
        if (from == address(0) || to == address(0)) revert ZeroAddress();
        if (from == to) revert SelfTransfer();
        if (from == MESSENGER) revert EscrowAccount(from);
        if (!canReceive(to)) revert ERC7943CannotReceive(to);
        uint256 u = unfrozenBalanceOf(from);
        if (amount > u) {
            _frozen[from] -= (amount - u);
            emit Frozen(from, _frozen[from]);
        }
        super._update(from, to, amount);
        emit ForcedTransfer(from, to, amount);
        return true;
    }

    /// @notice A holder lost its key: balance and freeze move to the
    ///         replacement, and the lost key is blocked for good.
    function recover(address lost, address replacement)
        external
        onlyRole(COMPLIANCE_ROLE)
        whenNotPaused
        returns (uint256 amount)
    {
        if (lost == address(0) || replacement == address(0)) revert ZeroAddress();
        if (lost == replacement) revert SelfTransfer();
        if (lost == MESSENGER) revert EscrowAccount(lost);
        if (!canReceive(replacement)) revert ERC7943CannotReceive(replacement);
        uint256 f = _frozen[lost];
        if (f != 0) {
            _frozen[lost] = 0;
            _frozen[replacement] += f;
            emit Frozen(lost, 0);
            emit Frozen(replacement, _frozen[replacement]);
        }
        blocked[lost] = true;
        amount = balanceOf(lost);
        if (amount > 0) super._update(lost, replacement, amount);
        emit KeyRecovered(lost, replacement, amount);
    }

    function pause() external onlyRole(PAUSER_ROLE) {
        _pause();
    }

    function unpause() external onlyRole(PAUSER_ROLE) {
        _unpause();
    }

    function setPolicy(IHolderPolicy p) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (address(p) == address(0)) revert ZeroAddress();
        policy = p;
        emit PolicyChanged(address(p));
    }

    function supportsInterface(bytes4 interfaceId)
        public
        view
        override(AccessControlDefaultAdminRules, IERC165)
        returns (bool)
    {
        return interfaceId == type(IERC7943FungibleToken).interfaceId || super.supportsInterface(interfaceId);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                The one pipeline
    //////////////////////////////////////////////////////////////////////////*/

    /// @dev Transfers need both sides admitted and an unfrozen balance; mints
    ///      need an admitted receiver; burns need an unfrozen balance.
    function _update(address from, address to, uint256 value) internal override {
        _requireNotPaused();
        if (from != address(0) && to != address(0)) {
            if (!canSend(from)) revert ERC7943CannotSend(from);
            if (!canReceive(to)) revert ERC7943CannotReceive(to);
        } else if (from == address(0)) {
            if (!canReceive(to)) revert ERC7943CannotReceive(to);
        }
        if (from != address(0)) {
            uint256 u = unfrozenBalanceOf(from);
            if (value > u) revert ERC7943InsufficientUnfrozenBalance(from, value, u);
        }
        super._update(from, to, value);
    }

}
