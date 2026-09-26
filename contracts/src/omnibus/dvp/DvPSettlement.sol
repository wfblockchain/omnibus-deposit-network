// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { AccessControlDefaultAdminRules } from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { ReentrancyGuard } from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";
import { EIP712 } from "@openzeppelin/contracts/utils/cryptography/EIP712.sol";
import { SignatureChecker } from "@openzeppelin/contracts/utils/cryptography/SignatureChecker.sol";
import { ERC165Checker } from "@openzeppelin/contracts/utils/introspection/ERC165Checker.sol";
import { IERC20 } from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import { SafeERC20 } from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import { IERC7943FungibleToken } from "../IERC7943.sol";
import { CrossChainMessenger } from "../crosschain/CrossChainMessenger.sol";

/**
 * @title DvPSettlement — delivery versus payment where the asset lives
 * @notice Settles a trade of a tokenized security against a bank's tokenized
 *         deposit on the chain where the security is issued. The cash comes
 *         there from the operator's home chain by burn and mint; the backing never
 *         leaves the joint account at the Fed. Each trade settles gross and
 *         both legs move in one transaction: DvP model 1 in the CPMI-IOSCO
 *         sense.
 *
 * @dev LIFECYCLE. Both parties affirm the same terms (directly or by an
 *      EIP-712 signature a bank or venue submits for them); the trade is then
 *      matched, an obligation. Each leg is either PULLED at settlement from
 *      the party's wallet (it approved this contract) or PRE-FUNDED into
 *      escrow before it. When both legs can move, `settle` moves both or
 *      neither, and any funding step tries it at once.
 *
 *      WHY BOTH MODES. Pull locks nothing and keeps the security out of
 *      escrow, so the venue never has to be an eligible holder of the
 *      security: the asset's own compliance checks the seller-to-buyer
 *      transfer, exactly as it would bilaterally. Pre-funding exists for cash
 *      that arrives from another chain before the seller is ready: the
 *      messenger mints it straight into this contract against the trade.
 *
 *      CASH FROM HOME, IN ONE STEP. At home a buyer's bank calls the
 *      messenger's depositForBurnWithHook with this contract as recipient and
 *      destination caller, and hookData = (tradeId, beneficiary). Here,
 *      `receiveCash` delivers the attested message itself, so the cash and the
 *      instruction arrive together and nobody can deliver one without the
 *      other. Cash that cannot fund its trade (unknown, already funded, wrong
 *      amount, past its deadline, venue paused) is never refused, since a
 *      refused message could never be delivered: it is credited to the
 *      beneficiary, who withdraws it or sends it home.
 *
 *      CANCELLATION follows AtomicDvP. Before the match an affirmation is an
 *      offer and its maker may withdraw it. A matched trade cancels only
 *      bilaterally (one party asks, the other consents) or lapses at its
 *      deadline, after which anyone may close it and return the escrow.
 *
 *      REFUNDS CANNOT STRAND. A closed trade pushes escrow back to whoever
 *      funded it. If that push fails (the funder has since been frozen or
 *      removed from the token's holder list), the amount is credited to the
 *      funder instead, so one blocked party never blocks the other's refund.
 *      Credited cash can be withdrawn here or sent back to the operator's home chain
 *      through the messenger, for a buyer not admitted on this chain.
 *
 *      WHAT THE OPERATOR CANNOT DO. There is no admin path to escrowed funds
 *      and no upgrade; a new version is a new deployment with a new EIP-712
 *      domain. Admin handover is two-step and delayed. Pausing stops new
 *      affirmations, funding and settlement; refunds, cancellations and
 *      withdrawals still work, so a pause never traps anyone's money. A
 *      regulator's order against funds in escrow goes through the issuing
 *      bank's forced transfer on the token itself, as for any holder.
 *
 *      FINALITY. The contract settles in one transaction; when that becomes
 *      final is the chain's property and the rulebook's decision (on Ethereum,
 *      the finalized block containing Settled). Attesters sign a cash message
 *      only once its burn is final at home, so cash never arrives here on a
 *      burn that could still be reorganised away.
 *
 *      AMOUNTS ARE EXACT. Terms state both amounts in each token's own units:
 *      no price, no rounding. A leg whose recipient does not receive exactly
 *      the stated amount (a fee-on-transfer or rebasing token) fails.
 */
contract DvPSettlement is AccessControlDefaultAdminRules, Pausable, ReentrancyGuard, EIP712 {

    using SafeERC20 for IERC20;

    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");

    bytes32 public constant TERMS_TYPEHASH = keccak256(
        "Terms(address seller,address buyer,address asset,uint256 assetAmount,address cash,uint256 cashAmount,uint64 settleBy,bytes32 ref,bytes32 salt)"
    );

    /// @notice What both parties agree. `ref` is the trade's settlement
    ///         reference (the sese.023 transaction id); `salt` makes two
    ///         otherwise identical trades distinct.
    struct Terms {
        address seller;
        address buyer;
        address asset;
        uint256 assetAmount;
        address cash;
        uint256 cashAmount;
        uint64 settleBy;
        bytes32 ref;
        bytes32 salt;
    }

    enum Status {
        None,
        Affirmed,
        Matched,
        Settled,
        Cancelled
    }

    struct Trade {
        Terms terms;
        Status status;
        bool sellerAffirmed;
        bool buyerAffirmed;
        address cancelRequestedBy;
        address assetFunder; // escrowed asset belongs to it until settlement
        address cashFunder; // escrowed cash belongs to it until settlement
    }

    /// @notice Why cash from another chain was credited instead of funding.
    enum CreditReason {
        UnknownOrClosedTrade,
        AlreadyFunded,
        WrongTokenOrAmount,
        PastDeadline,
        VenuePaused,
        MalformedInstruction
    }

    CrossChainMessenger public immutable MESSENGER;

    mapping(bytes32 tradeId => Trade) private _trades;

    /// @notice Tokens held against open trades, and credited cash awaiting
    ///         withdrawal. This contract's balance of a token is at least
    ///         their sum.
    mapping(address token => uint256) public escrowed;
    mapping(address token => uint256) public credited;
    mapping(address token => mapping(address account => uint256)) public credit;

    event Affirmed(bytes32 indexed tradeId, address indexed party);
    event Matched(bytes32 indexed tradeId, bytes32 indexed ref, Terms terms);
    event LegFunded(bytes32 indexed tradeId, address indexed token, address indexed funder, uint256 amount);
    event SettlementPending(bytes32 indexed tradeId, bytes reason);
    event Settled(bytes32 indexed tradeId, bytes32 indexed ref);
    event CancelRequested(bytes32 indexed tradeId, address indexed by);
    event Cancelled(bytes32 indexed tradeId, bytes32 indexed ref);
    event Refunded(bytes32 indexed tradeId, address indexed token, address indexed to, uint256 amount);
    event CashCredited(
        bytes32 indexed tradeId, address indexed beneficiary, address indexed token, uint256 amount, CreditReason reason
    );
    event CreditWithdrawn(address indexed account, address indexed token, uint256 amount);
    event CreditSentHome(address indexed account, address indexed token, uint256 amount, address homeRecipient, uint64 nonce);
    event RefundCredited(bytes32 indexed tradeId, address indexed token, address indexed to, uint256 amount);
    event Revoked(bytes32 indexed tradeId, address indexed party);

    error BadTerms();
    error PastDeadline(uint64 settleBy);
    error NotAParty(bytes32 tradeId, address account);
    error AlreadyAffirmed(bytes32 tradeId, address party);
    error WrongStatus(bytes32 tradeId, Status status);
    error BadSignature(bytes32 tradeId, address party);
    error LegAlreadyFunded(bytes32 tradeId, address token);
    error LegCannotMove(address token, address account);
    error InexactTransfer(address token, address to, uint256 expected, uint256 received);
    error NotRefundable(bytes32 tradeId);
    error CancelAlreadyRequested(bytes32 tradeId, address by);
    error OnlySelf();
    error NoMessenger();
    error NothingToWithdraw();
    error UnknownToken(bytes32 memberId);

    constructor(CrossChainMessenger messenger, address admin, uint48 adminDelay)
        AccessControlDefaultAdminRules(adminDelay, admin)
        EIP712("DvPSettlement", "1")
    {
        MESSENGER = messenger;
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Views
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The trade's id is the EIP-712 digest of its terms: bound to
    ///         this chain and this contract, and the message a party signs.
    function tradeId(Terms calldata t) public view returns (bytes32) {
        return _hashTypedDataV4(
            keccak256(
                abi.encode(
                    TERMS_TYPEHASH,
                    t.seller,
                    t.buyer,
                    t.asset,
                    t.assetAmount,
                    t.cash,
                    t.cashAmount,
                    t.settleBy,
                    t.ref,
                    t.salt
                )
            )
        );
    }

    function trade(bytes32 id) external view returns (Trade memory) {
        return _trades[id];
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Matching
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The caller, seller or buyer, affirms these terms.
    function affirm(Terms calldata t) external whenNotPaused returns (bytes32 id) {
        id = tradeId(t);
        _affirm(id, t, msg.sender);
    }

    /// @notice Anyone submits a party's EIP-712 signature over the terms: a
    ///         bank affirming for its client, or a venue matching both sides
    ///         in one transaction. Contract wallets sign by ERC-1271.
    function affirmFor(Terms calldata t, address party, bytes calldata signature)
        external
        whenNotPaused
        returns (bytes32 id)
    {
        id = tradeId(t);
        if (!SignatureChecker.isValidSignatureNow(party, id, signature)) revert BadSignature(id, party);
        _affirm(id, t, party);
    }

    function _affirm(bytes32 id, Terms calldata t, address party) internal {
        Trade storage tr = _trades[id];
        if (tr.status == Status.None) {
            _checkTerms(t);
            tr.terms = t;
            tr.status = Status.Affirmed;
        } else if (tr.status != Status.Affirmed) {
            revert WrongStatus(id, tr.status);
        }
        if (block.timestamp > t.settleBy) revert PastDeadline(t.settleBy);

        if (party == t.seller) {
            if (tr.sellerAffirmed) revert AlreadyAffirmed(id, party);
            tr.sellerAffirmed = true;
        } else if (party == t.buyer) {
            if (tr.buyerAffirmed) revert AlreadyAffirmed(id, party);
            tr.buyerAffirmed = true;
        } else {
            revert NotAParty(id, party);
        }
        emit Affirmed(id, party);

        if (tr.sellerAffirmed && tr.buyerAffirmed) {
            // Fail at the match, not at the deadline, when a leg can never
            // reach its receiver under the token's own rules.
            _preflight(t.asset, t.seller, t.buyer);
            _preflight(t.cash, address(0), t.seller);
            tr.status = Status.Matched;
            emit Matched(id, t.ref, t);
        }
    }

    function _checkTerms(Terms calldata t) internal view {
        if (
            t.seller == address(0) || t.buyer == address(0) || t.seller == t.buyer || t.asset == address(0)
                || t.cash == address(0) || t.asset == t.cash || t.assetAmount == 0 || t.cashAmount == 0
        ) revert BadTerms();
        if (block.timestamp > t.settleBy) revert PastDeadline(t.settleBy);
    }

    /// @dev Only tokens that report ERC-7943 can be asked; others are left to
    ///      fail, if they fail, at settlement.
    function _preflight(address token, address from, address to) internal view {
        if (!ERC165Checker.supportsInterface(token, type(IERC7943FungibleToken).interfaceId)) return;
        IERC7943FungibleToken k = IERC7943FungibleToken(token);
        if (from != address(0) && !k.canSend(from)) revert LegCannotMove(token, from);
        if (!k.canReceive(to)) revert LegCannotMove(token, to);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Funding
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The seller escrows the security ahead of settlement. Optional:
    ///         an unfunded asset leg is pulled at settlement.
    function fundAsset(bytes32 id) external whenNotPaused nonReentrant {
        Trade storage tr = _matchedParty(id, true);
        if (tr.assetFunder != address(0)) revert LegAlreadyFunded(id, tr.terms.asset);
        tr.assetFunder = msg.sender;
        _pullExact(tr.terms.asset, msg.sender, tr.terms.assetAmount);
        emit LegFunded(id, tr.terms.asset, msg.sender, tr.terms.assetAmount);
        _trySettle(id);
    }

    /// @notice The buyer escrows the cash from its wallet on this chain.
    function fundCash(bytes32 id) external whenNotPaused nonReentrant {
        Trade storage tr = _matchedParty(id, false);
        if (tr.cashFunder != address(0)) revert LegAlreadyFunded(id, tr.terms.cash);
        tr.cashFunder = msg.sender;
        _pullExact(tr.terms.cash, msg.sender, tr.terms.cashAmount);
        emit LegFunded(id, tr.terms.cash, msg.sender, tr.terms.cashAmount);
        _trySettle(id);
    }

    /// @notice Delivers an attested cross-chain message whose tokens mint to
    ///         this contract, and applies its instruction: fund the named
    ///         trade's cash leg and try to settle, or credit the beneficiary.
    /// @dev Never reverts for a business reason, so a burned-at-home message
    ///      is always deliverable. Works while paused (as a credit).
    function receiveCash(bytes calldata message, bytes[] calldata signatures, bytes calldata issuerSignature)
        external
        nonReentrant
    {
        if (address(MESSENGER) == address(0)) revert NoMessenger();
        CrossChainMessenger.Delivery memory d = MESSENGER.receiveMessage(message, signatures, issuerSignature);
        if (d.recipient != address(this)) return; // minted elsewhere: nothing arrived here

        bytes32 id;
        address beneficiary = d.sender;
        CreditReason why;
        bool credit_ = true;
        if (d.hookData.length != 64) {
            why = CreditReason.MalformedInstruction;
        } else {
            address b;
            (id, b) = abi.decode(d.hookData, (bytes32, address));
            if (b != address(0)) beneficiary = b;
            Trade storage tr = _trades[id];
            if (paused()) {
                why = CreditReason.VenuePaused;
            } else if (tr.status != Status.Matched) {
                why = CreditReason.UnknownOrClosedTrade;
            } else if (tr.cashFunder != address(0)) {
                why = CreditReason.AlreadyFunded;
            } else if (d.token != tr.terms.cash || d.amount != tr.terms.cashAmount) {
                why = CreditReason.WrongTokenOrAmount;
            } else if (block.timestamp > tr.terms.settleBy) {
                why = CreditReason.PastDeadline;
            } else {
                credit_ = false;
                tr.cashFunder = beneficiary;
                escrowed[d.token] += d.amount;
                emit LegFunded(id, d.token, beneficiary, d.amount);
            }
        }
        if (credit_) {
            credit[d.token][beneficiary] += d.amount;
            credited[d.token] += d.amount;
            emit CashCredited(id, beneficiary, d.token, d.amount, why);
            return;
        }
        _trySettle(id);
    }

    /// @notice Withdraws cash credited to the caller. Works while paused.
    function withdrawCredit(address token) external nonReentrant {
        uint256 a = credit[token][msg.sender];
        if (a == 0) revert NothingToWithdraw();
        credit[token][msg.sender] = 0;
        credited[token] -= a;
        IERC20(token).safeTransfer(msg.sender, a);
        emit CreditWithdrawn(msg.sender, token, a);
    }

    /// @notice Sends cash credited to the caller back to the operator's home chain,
    ///         to `homeRecipient`, where the bank can redeem it. Works while
    ///         this venue is paused (the messenger has its own pause).
    function withdrawCreditHome(bytes32 memberId, address homeRecipient) external nonReentrant {
        if (address(MESSENGER) == address(0)) revert NoMessenger();
        address token = MESSENGER.localToken(memberId);
        if (token == address(0)) revert UnknownToken(memberId);
        uint256 a = credit[token][msg.sender];
        if (a == 0) revert NothingToWithdraw();
        credit[token][msg.sender] = 0;
        credited[token] -= a;
        // If home cannot deliver (the recipient is not admitted there), the
        // bounce returns the amount to the caller on this chain, not to the venue.
        uint64 nonce =
            MESSENGER.depositForBurnWithHook(memberId, a, MESSENGER.HOME_DOMAIN(), homeRecipient, address(0), "", msg.sender);
        emit CreditSentHome(msg.sender, token, a, homeRecipient, nonce);
    }

    function _matchedParty(bytes32 id, bool seller) internal view returns (Trade storage tr) {
        tr = _trades[id];
        if (tr.status != Status.Matched) revert WrongStatus(id, tr.status);
        if (block.timestamp > tr.terms.settleBy) revert PastDeadline(tr.terms.settleBy);
        if (msg.sender != (seller ? tr.terms.seller : tr.terms.buyer)) revert NotAParty(id, msg.sender);
    }

    function _pullExact(address token, address from, uint256 amount) internal {
        uint256 before = IERC20(token).balanceOf(address(this));
        IERC20(token).safeTransferFrom(from, address(this), amount);
        uint256 got = IERC20(token).balanceOf(address(this)) - before;
        if (got != amount) revert InexactTransfer(token, address(this), amount, got);
        escrowed[token] += amount;
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Settlement
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Settles a matched trade: both legs, or neither. Anyone may
    ///         trigger it; both parties consented at the match.
    function settle(bytes32 id) external whenNotPaused nonReentrant {
        _settle(id);
    }

    /// @notice Best effort over many trades; returns which ones settled.
    function settleMany(bytes32[] calldata ids) external whenNotPaused nonReentrant returns (bool[] memory ok) {
        ok = new bool[](ids.length);
        for (uint256 i = 0; i < ids.length; i++) {
            try this.settleFromSelf(ids[i]) {
                ok[i] = true;
            } catch (bytes memory reason) {
                emit SettlementPending(ids[i], reason);
            }
        }
    }

    /// @dev The try/catch target: a failed attempt rolls back only itself.
    function settleFromSelf(bytes32 id) external {
        if (msg.sender != address(this)) revert OnlySelf();
        _settle(id);
    }

    function _trySettle(bytes32 id) internal {
        try this.settleFromSelf(id) { }
        catch (bytes memory reason) {
            emit SettlementPending(id, reason);
        }
    }

    /// @dev Status is written before any transfer, so a reentrant token finds
    ///      the trade settled. Each leg is checked at its receiver.
    function _settle(bytes32 id) internal {
        Trade storage tr = _trades[id];
        if (tr.status != Status.Matched) revert WrongStatus(id, tr.status);
        Terms memory t = tr.terms;
        if (block.timestamp > t.settleBy) revert PastDeadline(t.settleBy);
        tr.status = Status.Settled;

        _moveLeg(t.asset, tr.assetFunder, t.seller, t.buyer, t.assetAmount);
        _moveLeg(t.cash, tr.cashFunder, t.buyer, t.seller, t.cashAmount);
        emit Settled(id, t.ref);
    }

    function _moveLeg(address token, address funder, address from, address to, uint256 amount) internal {
        uint256 before = IERC20(token).balanceOf(to);
        if (funder != address(0)) {
            escrowed[token] -= amount;
            IERC20(token).safeTransfer(to, amount);
        } else {
            // `from` is the trade's seller or buyer, who affirmed these exact terms.
            // slither-disable-next-line arbitrary-send-erc20
            IERC20(token).safeTransferFrom(from, to, amount);
        }
        uint256 got = IERC20(token).balanceOf(to) - before;
        if (got != amount) revert InexactTransfer(token, to, amount, got);
    }

    /*//////////////////////////////////////////////////////////////////////////
                            Cancellation and refunds
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Affirmed: the party that affirmed withdraws its offer. Matched:
    ///         one party asks, the other consents. Escrow returns at once.
    ///         Works while paused.
    function cancel(bytes32 id) external nonReentrant {
        Trade storage tr = _trades[id];
        Terms memory t = tr.terms;
        if (msg.sender != t.seller && msg.sender != t.buyer) revert NotAParty(id, msg.sender);

        if (tr.status == Status.Affirmed) {
            bool mine = msg.sender == t.seller ? tr.sellerAffirmed : tr.buyerAffirmed;
            if (!mine) revert NotAParty(id, msg.sender);
            _close(id, tr);
            return;
        }
        if (tr.status != Status.Matched) revert WrongStatus(id, tr.status);
        if (block.timestamp > t.settleBy) {
            _close(id, tr);
            return;
        }
        if (tr.cancelRequestedBy == address(0)) {
            tr.cancelRequestedBy = msg.sender;
            emit CancelRequested(id, msg.sender);
            return;
        }
        if (tr.cancelRequestedBy == msg.sender) revert CancelAlreadyRequested(id, msg.sender);
        _close(id, tr);
    }

    /// @notice A party kills terms it signed but nobody has submitted yet,
    ///         so a leaked or stale signature can never affirm them.
    function revoke(Terms calldata t) external {
        if (msg.sender != t.seller && msg.sender != t.buyer) revert NotAParty(bytes32(0), msg.sender);
        bytes32 id = tradeId(t);
        Trade storage tr = _trades[id];
        if (tr.status != Status.None) revert WrongStatus(id, tr.status);
        tr.status = Status.Cancelled;
        emit Revoked(id, msg.sender);
        emit Cancelled(id, t.ref);
    }

    /// @notice A matched trade past its deadline has lapsed: anyone closes it
    ///         and the escrow goes back to whoever funded it. Works while
    ///         paused.
    function refund(bytes32 id) external nonReentrant {
        Trade storage tr = _trades[id];
        if (tr.status != Status.Matched || block.timestamp <= tr.terms.settleBy) revert NotRefundable(id);
        _close(id, tr);
    }

    // Reached only from cancel and refund, both nonReentrant; status is written
    // first and the self-call targets refuse any caller but this contract.
    // slither-disable-next-line reentrancy-no-eth
    function _close(bytes32 id, Trade storage tr) internal {
        tr.status = Status.Cancelled;
        Terms memory t = tr.terms;
        address af = tr.assetFunder;
        address cf = tr.cashFunder;
        tr.assetFunder = address(0);
        tr.cashFunder = address(0);
        if (af != address(0)) _giveBack(id, t.asset, af, t.assetAmount);
        if (cf != address(0)) _giveBack(id, t.cash, cf, t.cashAmount);
        emit Cancelled(id, t.ref);
    }

    /// @dev Push the escrow back; if the token refuses (the funder is frozen
    ///      or no longer admitted), credit it instead of reverting.
    function _giveBack(bytes32 id, address token, address to, uint256 amount) internal {
        escrowed[token] -= amount;
        try this.pushFromSelf(token, to, amount) {
            emit Refunded(id, token, to, amount);
        } catch {
            credit[token][to] += amount;
            credited[token] += amount;
            emit RefundCredited(id, token, to, amount);
        }
    }

    /// @dev The try/catch target for refunds.
    function pushFromSelf(address token, address to, uint256 amount) external {
        if (msg.sender != address(this)) revert OnlySelf();
        IERC20(token).safeTransfer(to, amount);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Pause
    //////////////////////////////////////////////////////////////////////////*/

    function pause() external onlyRole(PAUSER_ROLE) {
        _pause();
    }

    function unpause() external onlyRole(PAUSER_ROLE) {
        _unpause();
    }

}
