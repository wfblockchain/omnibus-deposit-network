// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { AccessControlDefaultAdminRules } from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { OmnibusLedger } from "./OmnibusLedger.sol";
import { BankToken } from "./BankToken.sol";

/**
 * @title PaymentRouter
 * @notice Cross-bank payments and swaps between member tickers: burn the
 *         payer's ticker, move the backing inside the operator's omnibus position
 *         ledger, mint the payee's ticker. One transaction or none, at par,
 *         with no rate anywhere.
 *
 * @dev THE RTP LIFECYCLE, ON-CHAIN.
 *        pay()     ≈ pacs.008 credit transfer from the sending bank's customer
 *        accept()  ≈ pacs.002 ACSC from the receiving bank: settles
 *        reject()  ≈ pacs.002 RJCT with an ISO reason code: releases the hold
 *        expire()  ≈ the receiver-response timeout: releases the hold
 *        returnPayment()  ≈ the payee's return: in RTP a new pacs.008 that
 *                          references the original (RTP has no pacs.004)
 *        requestReturn()  ≈ camt.056 Request for Return of Funds; the payee
 *                          answers with camt.029 and is under no obligation
 *
 *      A receiving member either accepts inbound payments explicitly (its
 *      gateway screens the payee and the payer, then calls accept) or sets
 *      requiresAcceptance = false, in which case its holder policy is the
 *      acceptance and payments settle in the pay() call. RTP gives a
 *      receiver about five seconds; on-chain acceptance needs a block or
 *      two, so the default window here is 30 seconds. RTP's "accept without
 *      posting" (ACWP) maps to accept followed by the receiving bank's
 *      compliance freeze on the credited tokens while it finishes a review.
 *
 *      WHO CAN MOVE WHOSE MONEY. The router debits only msg.sender: the payer
 *      in pay(), the original payee in returnPayment(). It holds no general
 *      debit power over accounts, unlike the SETTLEMENT_ROLE of the earlier
 *      model.
 *
 *      IDS. A payment id is derived from the payer and its own reference
 *      (the UETR the payer's bank assigned), so nobody else can occupy an id
 *      a payer intends to use.
 *
 *      Same-ticker payments are ordinary ERC-20 transfers and do not come
 *      through here: the reserves do not move, so there is nothing for the operator
 *      to do.
 */
contract PaymentRouter is AccessControlDefaultAdminRules, Pausable {

    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");

    uint32 public constant MIN_ACCEPT_WINDOW = 5 seconds;
    uint32 public constant MAX_ACCEPT_WINDOW = 1 hours;

    OmnibusLedger public immutable LEDGER;

    /// @notice How long a receiving member has to accept or reject.
    uint32 public acceptWindow = 30 seconds;

    enum Status {
        None,
        Pending,
        Settled,
        Rejected,
        Expired
    }

    struct Payment {
        address payer;
        address payee;
        BankToken fromToken;
        BankToken toToken;
        uint256 amount;
        uint64 deadline;
        Status status;
        bytes32 returnOf; // non-zero for a return
        uint256 returned; // total returned against this payment
    }

    mapping(bytes32 paymentId => Payment) internal _payments;

    event PaymentPending(
        bytes32 indexed paymentId,
        address indexed payer,
        address indexed payee,
        address fromToken,
        address toToken,
        uint256 amount,
        uint64 deadline
    );
    event PaymentSettled(
        bytes32 indexed paymentId,
        address indexed payer,
        address indexed payee,
        address fromToken,
        address toToken,
        uint256 amount
    );
    event PaymentRejected(bytes32 indexed paymentId, bytes4 reasonCode);
    event PaymentExpired(bytes32 indexed paymentId);
    event PaymentReturned(bytes32 indexed returnId, bytes32 indexed originalId, uint256 amount, bytes4 reasonCode);
    event ReturnRequested(bytes32 indexed originalId, address indexed by, bytes4 reasonCode);
    event AcceptWindowChanged(uint32 window);
    event HeldTokensSettled(address indexed token, address indexed holder, bytes32 indexed holderMember, uint256 amount);

    error SameTicker(address token);
    error UnknownTicker(address token);
    error ReceiverSuspended(bytes32 memberId);
    error PayeeNotAdmitted(address payee);
    error ZeroAmount();
    error DuplicatePayment(bytes32 paymentId);
    error NotPending(bytes32 paymentId, Status status);
    error NotReceivingOperator(bytes32 paymentId, address caller);
    error PastDeadline(bytes32 paymentId, uint64 deadline);
    error BeforeDeadline(bytes32 paymentId, uint64 deadline);
    error NotSettled(bytes32 paymentId);
    error NotThePayee(bytes32 paymentId, address caller);
    error NotThePayer(bytes32 paymentId, address caller);
    error ReturnExceedsPayment(bytes32 paymentId, uint256 requested, uint256 remaining);
    error BadWindow(uint32 window);
    error NotAMemberWallet(address caller);
    error OwnTicker(address token);
    error HoldMismatch(bytes32 paymentId, address account, uint256 amount);

    constructor(OmnibusLedger ledger, address admin, uint48 adminDelay) AccessControlDefaultAdminRules(adminDelay, admin) {
        LEDGER = ledger;
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Payments
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Pays `payee` in `toToken` out of the caller's `fromToken`.
    ///         With payee == caller it is a swap between tickers.
    /// @param clientRef The payer's own reference (its bank's UETR).
    function pay(BankToken fromToken, BankToken toToken, address payee, uint256 amount, bytes32 clientRef)
        external
        whenNotPaused
        returns (bytes32 paymentId, Status status)
    {
        if (amount == 0) revert ZeroAmount();
        if (address(fromToken) == address(toToken)) revert SameTicker(address(fromToken));
        (bytes32 fromId, bytes32 toId, bool needsAccept) = _route(fromToken, toToken);
        if (!toToken.canReceive(payee)) revert PayeeNotAdmitted(payee);

        paymentId = keccak256(abi.encode(block.chainid, address(this), msg.sender, clientRef));
        if (_payments[paymentId].status != Status.None) revert DuplicatePayment(paymentId);

        Payment storage p = _payments[paymentId];
        p.payer = msg.sender;
        p.payee = payee;
        p.fromToken = fromToken;
        p.toToken = toToken;
        p.amount = amount;

        // State first, then the token calls (checks-effects-interactions).
        if (needsAccept) {
            p.deadline = uint64(block.timestamp) + acceptWindow;
            p.status = Status.Pending;
            fromToken.hold(msg.sender, amount, paymentId);
            emit PaymentPending(
                paymentId, msg.sender, payee, address(fromToken), address(toToken), amount, p.deadline
            );
        } else {
            p.status = Status.Settled;
            fromToken.routerBurn(msg.sender, amount);
            LEDGER.moveBacking(fromId, toId, amount);
            toToken.routerMint(payee, amount);
            emit PaymentSettled(paymentId, msg.sender, payee, address(fromToken), address(toToken), amount);
        }
        return (paymentId, p.status);
    }

    /// @notice The receiving member accepts (pacs.002 ACSC) and the payment
    ///         settles now: held tokens burn, backing moves, payee is minted.
    function accept(bytes32 paymentId) external whenNotPaused {
        Payment storage p = _pending(paymentId);
        bytes32 toId = LEDGER.memberOfToken(address(p.toToken));
        if (msg.sender != LEDGER.member(toId).operator) revert NotReceivingOperator(paymentId, msg.sender);
        if (block.timestamp > p.deadline) revert PastDeadline(paymentId, p.deadline);

        p.status = Status.Settled;
        (address heldFrom, uint256 heldAmount) = p.fromToken.burnHeld(paymentId);
        if (heldFrom != p.payer || heldAmount != p.amount) revert HoldMismatch(paymentId, heldFrom, heldAmount);
        LEDGER.moveBacking(LEDGER.memberOfToken(address(p.fromToken)), toId, p.amount);
        p.toToken.routerMint(p.payee, p.amount);
        emit PaymentSettled(paymentId, p.payer, p.payee, address(p.fromToken), address(p.toToken), p.amount);
    }

    /// @notice The receiving member rejects (pacs.002 RJCT) with an ISO
    ///         20022 reason code, e.g. "AC04" closed account, "RR04"
    ///         regulatory. The payer's hold is released.
    function reject(bytes32 paymentId, bytes4 reasonCode) external {
        Payment storage p = _pending(paymentId);
        bytes32 toId = LEDGER.memberOfToken(address(p.toToken));
        if (msg.sender != LEDGER.member(toId).operator) revert NotReceivingOperator(paymentId, msg.sender);
        p.status = Status.Rejected;
        p.fromToken.releaseHold(paymentId);
        emit PaymentRejected(paymentId, reasonCode);
    }

    /// @notice No answer by the deadline: anyone may release the hold. Works
    ///         while paused, so a pause never traps a payer's funds.
    function expire(bytes32 paymentId) external {
        Payment storage p = _pending(paymentId);
        if (block.timestamp <= p.deadline) revert BeforeDeadline(paymentId, p.deadline);
        p.status = Status.Expired;
        p.fromToken.releaseHold(paymentId);
        emit PaymentExpired(paymentId);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Returns
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The original payee sends funds back, in the ticker
    ///         it received, to the original payer in the ticker it paid
    ///         from. Returns settle immediately: the originating bank already
    ///         knows its customer. They can never exceed the original.
    function returnPayment(bytes32 originalId, uint256 amount, bytes4 reasonCode, bytes32 clientRef)
        external
        whenNotPaused
        returns (bytes32 returnId)
    {
        Payment storage o = _payments[originalId];
        if (o.status != Status.Settled) revert NotSettled(originalId);
        if (msg.sender != o.payee) revert NotThePayee(originalId, msg.sender);
        if (amount == 0) revert ZeroAmount();
        uint256 remaining = o.amount - o.returned;
        if (amount > remaining) revert ReturnExceedsPayment(originalId, amount, remaining);

        BankToken fromToken = o.toToken;
        BankToken toToken = o.fromToken;
        (bytes32 fromId, bytes32 toId,) = _route(fromToken, toToken);
        if (!toToken.canReceive(o.payer)) revert PayeeNotAdmitted(o.payer);

        returnId = keccak256(abi.encode(block.chainid, address(this), msg.sender, clientRef));
        if (_payments[returnId].status != Status.None) revert DuplicatePayment(returnId);
        o.returned += amount;

        Payment storage r = _payments[returnId];
        r.payer = msg.sender;
        r.payee = o.payer;
        r.fromToken = fromToken;
        r.toToken = toToken;
        r.amount = amount;
        r.returnOf = originalId;
        r.status = Status.Settled;

        fromToken.routerBurn(msg.sender, amount);
        LEDGER.moveBacking(fromId, toId, amount);
        toToken.routerMint(o.payer, amount);
        emit PaymentSettled(returnId, msg.sender, o.payer, address(fromToken), address(toToken), amount);
        emit PaymentReturned(returnId, originalId, amount, reasonCode);
    }

    /// @notice The original payer asks for its money back (camt.056). A
    ///         request only; the payee decides whether to return.
    function requestReturn(bytes32 originalId, bytes4 reasonCode) external {
        Payment storage o = _payments[originalId];
        if (o.status != Status.Settled) revert NotSettled(originalId);
        if (msg.sender != o.payer) revert NotThePayer(originalId, msg.sender);
        emit ReturnRequested(originalId, msg.sender, reasonCode);
    }

    /*//////////////////////////////////////////////////////////////////////////
                        Interbank settlement of held tokens
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice A member bank settles another member's tokenized deposits it
    ///         holds in its settlement wallet: they are burned and the
    ///         issuer's reserves under them become the holder's free
    ///         position, which it can mint its own ticker against or defund.
    ///         Gross, one presentation at a time.
    function settleHeld(BankToken token, uint256 amount) external whenNotPaused {
        if (amount == 0) revert ZeroAmount();
        bytes32 holder = LEDGER.memberOfWallet(msg.sender);
        if (holder == bytes32(0)) revert NotAMemberWallet(msg.sender);
        bytes32 issuer = LEDGER.memberOfToken(address(token));
        if (issuer == bytes32(0)) revert UnknownTicker(address(token));
        if (issuer == holder) revert OwnTicker(address(token));
        token.routerBurn(msg.sender, amount);
        LEDGER.settleHeldTokens(issuer, holder, amount);
        emit HeldTokensSettled(address(token), msg.sender, holder, amount);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Admin
    //////////////////////////////////////////////////////////////////////////*/

    function setAcceptWindow(uint32 window) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (window < MIN_ACCEPT_WINDOW || window > MAX_ACCEPT_WINDOW) revert BadWindow(window);
        acceptWindow = window;
        emit AcceptWindowChanged(window);
    }

    function pause() external onlyRole(PAUSER_ROLE) {
        _pause();
    }

    function unpause() external onlyRole(PAUSER_ROLE) {
        _unpause();
    }

    function payment(bytes32 paymentId) external view returns (Payment memory) {
        return _payments[paymentId];
    }

    /// @notice The id a payer's reference will produce, so a bank gateway
    ///         can index a payment before submitting it.
    function paymentIdFor(address payer, bytes32 clientRef) external view returns (bytes32) {
        return keccak256(abi.encode(block.chainid, address(this), payer, clientRef));
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Internals
    //////////////////////////////////////////////////////////////////////////*/

    function _route(BankToken fromToken, BankToken toToken)
        internal
        view
        returns (bytes32 fromId, bytes32 toId, bool needsAccept)
    {
        fromId = LEDGER.memberOfToken(address(fromToken));
        toId = LEDGER.memberOfToken(address(toToken));
        if (fromId == bytes32(0)) revert UnknownTicker(address(fromToken));
        if (toId == bytes32(0)) revert UnknownTicker(address(toToken));
        OmnibusLedger.Member memory t = LEDGER.member(toId);
        if (t.suspended) revert ReceiverSuspended(toId);
        needsAccept = t.requiresAcceptance;
    }

    function _pending(bytes32 paymentId) internal view returns (Payment storage p) {
        p = _payments[paymentId];
        if (p.status != Status.Pending) revert NotPending(paymentId, p.status);
    }

}
