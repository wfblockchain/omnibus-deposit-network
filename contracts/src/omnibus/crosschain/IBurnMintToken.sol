// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice The legs a messenger may call on a ticker, on the home chain
///         (BankToken) or on another chain (RemoteBankToken).
/// @dev Lock-then-burn: a move out locks the sender's tokens in the
///      messenger's escrow; the escrow burns once the destination has proven
///      its mint, or is released if the destination cancels. The escrow is
///      the messenger's own balance, reachable only through these legs: the
///      messenger is never an ordinary holder, so nobody can transfer to or
///      from it.
interface IBurnMintToken {

    /// @notice Moves `amount` of `from`'s available balance into the
    ///         messenger's escrow. The messenger calls it only for the sender
    ///         that called it.
    function messengerLock(address from, uint256 amount) external;

    /// @notice Burns `amount` of the messenger's escrow: its mint elsewhere is proven.
    function messengerBurnLocked(uint256 amount) external;

    /// @notice Returns `amount` of the messenger's escrow to `to`, which must
    ///         be able to hold the token.
    function messengerUnlock(address to, uint256 amount) external;

    /// @notice Mints on an attested message from another chain.
    function messengerMint(address to, uint256 amount) external;

}
