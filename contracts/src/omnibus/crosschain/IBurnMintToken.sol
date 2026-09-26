// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice The burn and mint legs a messenger may call on a ticker, on the
///         home chain (BankToken) or on another chain (RemoteBankToken).
interface IBurnMintToken {

    function messengerBurn(address from, uint256 amount) external;
    function messengerMint(address to, uint256 amount) external;

}
