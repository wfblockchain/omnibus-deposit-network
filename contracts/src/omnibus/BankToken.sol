// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { ERC20 } from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import { AccessControlDefaultAdminRules } from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { IERC165 } from "@openzeppelin/contracts/utils/introspection/IERC165.sol";
import { IERC7943FungibleToken } from "./IERC7943.sol";
import { IHolderPolicy } from "./HolderRegistry.sol";
import { OmnibusLedger } from "./OmnibusLedger.sol";
import { IBurnMintToken } from "./crosschain/IBurnMintToken.sol";

/**
 * @title BankToken
 * @notice One member bank's own dollar ticker (dtA, dtB), issued by
 *         that bank and backed 1:1 by its position in the operator's joint account
 *         at the Fed.
 *
 * @dev WHAT THE BANK CONTROLS. Who may hold its ticker (its holder policy),
 *      minting against deposits it debits in its core system, and its own
 *      freezes. WHAT IT DOES NOT CONTROL. The reserves under outstanding
 *      tokens: every mint encumbers the bank's position in the
 *      OmnibusLedger, and encumbered reserves cannot be defunded. They
 *      leave the bank's position only together with the tokens, when a
 *      holder pays a customer of another member.
 *
 *      THREE WAYS VALUE MOVES.
 *        - Same ticker (on-us): a plain ERC-20 transfer. The reserves stay
 *          in the issuer's position; nothing changes at the operator.
 *        - Cross ticker (a payment to another bank's customer, or a swap):
 *          through the PaymentRouter, which burns here, moves the backing
 *          inside the omnibus, and mints the other ticker. One transaction.
 *        - Redemption: the holder burns and the bank credits a deposit.
 *
 *      HOLDS, NOT ESCROW. A payment awaiting the receiving bank's acceptance
 *      places a hold on the payer's balance, the way RTP reserves the
 *      sender's position. The tokens do not move until the payment settles.
 *
 *      CONTROLS carry the fixes from the audit of the earlier model:
 *      forced transfers refuse the zero address on either side (no minting
 *      through compliance), burns respect freezes, every change to a frozen
 *      amount emits Frozen, recovery blocks the lost key for good, and the
 *      ERC-7943 interface is reported through ERC-165.
 */
contract BankToken is ERC20, AccessControlDefaultAdminRules, Pausable, IERC7943FungibleToken, IBurnMintToken {

    /// @notice The bank's mint/redeem service, bound to its core ledger.
    bytes32 public constant ISSUER_ROLE = keccak256("ISSUER_ROLE");

    /// @notice The bank's compliance function.
    bytes32 public constant COMPLIANCE_ROLE = keccak256("COMPLIANCE_ROLE");

    /// @notice The cross-chain messenger: escrows tokens leaving for another
    ///         chain, burns that escrow once their mint there is proven (or
    ///         returns it if the move is cancelled), and mints tokens
    ///         arriving from one. It never touches the ledger's encumbrance;
    ///         backing stays in the omnibus. A messenger is never an ordinary
    ///         holder: its escrow moves only through the messenger legs.
    bytes32 public constant MESSENGER_ROLE = keccak256("MESSENGER_ROLE");

    /// @notice The issuing bank's emergency stop: no transfer, mint, burn,
    ///         redemption, new hold, forced transfer or recovery while paused.
    ///         Freezing and releasing holds still work. Forced transfers stop
    ///         too because pause is also the kill switch for a compromised
    ///         compliance key (the fix to ERC-3643 audit finding M-09).
    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");

    OmnibusLedger public immutable LEDGER;
    address public immutable ROUTER;

    IHolderPolicy public policy;

    struct Hold {
        address account;
        uint256 amount;
    }

    mapping(address account => uint256) private _frozen;
    mapping(address account => uint256) private _held;
    mapping(bytes32 holdId => Hold) public holds;
    mapping(address account => bool) public blocked;
    mapping(bytes32 ref => bool) public refUsed;

    /// @notice Tokens reserved by all open holds; the ledger will not replace
    ///         this ticker while any exist.
    uint256 public totalHeld;

    /// @notice Set by the ledger when this ticker is replaced (or abandoned
    ///         as a successor): nothing can ever move it again.
    bool public retired;

    /// @notice Holders whose balance this ticker re-created from its
    ///         predecessor.
    mapping(address holder => bool) public migrated;

    event Minted(address indexed to, uint256 amount, bytes32 indexed ref);
    event Redeemed(address indexed holder, uint256 amount, bytes32 indexed ref);
    event HoldPlaced(bytes32 indexed holdId, address indexed account, uint256 amount);
    event HoldReleased(bytes32 indexed holdId);
    event HoldSettled(bytes32 indexed holdId);
    event PolicyChanged(address indexed policy);
    event KeyRecovered(address indexed lost, address indexed replacement, uint256 amount);
    event Retired();
    event BalanceMigrated(address indexed holder, uint256 amount, uint256 frozen);

    error OnlyRouter(address caller);
    error ZeroAddress();
    error ZeroAmount();
    error RefAlreadyUsed(bytes32 ref);
    error InsufficientAvailable(address account, uint256 amount, uint256 available);
    error DuplicateHold(bytes32 holdId);
    error UnknownHold(bytes32 holdId);
    error SelfTransfer();
    error HoldsOutstanding(address account, uint256 held);
    error OnlyLedger(address caller);
    error TokenRetired();
    error EscrowAccount(address account);

    constructor(
        string memory name_,
        string memory symbol_,
        OmnibusLedger ledger,
        address router,
        IHolderPolicy policy_,
        address admin,
        uint48 adminDelay
    ) ERC20(name_, symbol_) AccessControlDefaultAdminRules(adminDelay, admin) {
        if (address(ledger) == address(0) || router == address(0) || address(policy_) == address(0)) {
            revert ZeroAddress();
        }
        LEDGER = ledger;
        ROUTER = router;
        policy = policy_;
    }

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    modifier onlyRouter() {
        if (msg.sender != ROUTER) revert OnlyRouter(msg.sender);
        _;
    }

    /*//////////////////////////////////////////////////////////////////////////
                            Issuance and redemption
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The bank debited a customer's deposit (ref = its core
    ///         posting id) and issues the same amount in tokens, encumbering
    ///         the same amount of its omnibus position.
    function mint(address to, uint256 amount, bytes32 ref) external onlyRole(ISSUER_ROLE) {
        if (amount == 0) revert ZeroAmount();
        if (refUsed[ref]) revert RefAlreadyUsed(ref);
        refUsed[ref] = true;
        LEDGER.encumber(amount);
        _mint(to, amount);
        emit Minted(to, amount, ref);
    }

    /// @notice The holder returns tokens to the bank for a deposit credit.
    function redeem(uint256 amount, bytes32 ref) external {
        _redeem(msg.sender, amount, ref);
    }

    /// @notice The bank redeems on a holder's instruction, within an
    ///         allowance the holder granted. No allowance, no redemption.
    function redeemFrom(address holder, uint256 amount, bytes32 ref) external onlyRole(ISSUER_ROLE) {
        _spendAllowance(holder, msg.sender, amount);
        _redeem(holder, amount, ref);
    }

    function _redeem(address holder, uint256 amount, bytes32 ref) internal {
        if (amount == 0) revert ZeroAmount();
        if (refUsed[ref]) revert RefAlreadyUsed(ref);
        refUsed[ref] = true;
        uint256 a = availableBalanceOf(holder);
        if (amount > a) revert InsufficientAvailable(holder, amount, a);
        _burn(holder, amount);
        LEDGER.release(amount);
        emit Redeemed(holder, amount, ref);
    }

    /*//////////////////////////////////////////////////////////////////////////
                        Router legs (cross-ticker payments)
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Reserves part of a payer's balance for a payment awaiting the
    ///         receiving bank. The router places holds only for the payer
    ///         that called it.
    function hold(address account, uint256 amount, bytes32 holdId) external onlyRouter whenNotPaused {
        if (!canSend(account)) revert ERC7943CannotSend(account);
        if (holds[holdId].account != address(0)) revert DuplicateHold(holdId);
        uint256 a = availableBalanceOf(account);
        if (amount == 0) revert ZeroAmount();
        if (amount > a) revert InsufficientAvailable(account, amount, a);
        _held[account] += amount;
        totalHeld += amount;
        holds[holdId] = Hold(account, amount);
        emit HoldPlaced(holdId, account, amount);
    }

    function releaseHold(bytes32 holdId) external onlyRouter {
        Hold memory h = holds[holdId];
        if (h.account == address(0)) revert UnknownHold(holdId);
        _held[h.account] -= h.amount;
        totalHeld -= h.amount;
        delete holds[holdId];
        emit HoldReleased(holdId);
    }

    /// @notice Settles a held payment: the held tokens burn. A freeze placed
    ///         after the hold still binds here, because the burn checks the
    ///         unfrozen balance.
    function burnHeld(bytes32 holdId) external onlyRouter returns (address account, uint256 amount) {
        Hold memory h = holds[holdId];
        if (h.account == address(0)) revert UnknownHold(holdId);
        _held[h.account] -= h.amount;
        totalHeld -= h.amount;
        delete holds[holdId];
        _burn(h.account, h.amount);
        emit HoldSettled(holdId);
        return (h.account, h.amount);
    }

    /// @notice Instant cross-ticker payment, burn leg. The router calls it
    ///         only for the payer that called the router.
    function routerBurn(address from, uint256 amount) external onlyRouter {
        if (!canSend(from)) revert ERC7943CannotSend(from);
        uint256 a = availableBalanceOf(from);
        if (amount > a) revert InsufficientAvailable(from, amount, a);
        _burn(from, amount);
    }

    /// @notice Mint leg. The backing already moved into this bank's position.
    function routerMint(address to, uint256 amount) external onlyRouter {
        _mint(to, amount);
    }

    /*//////////////////////////////////////////////////////////////////////////
            Cross-chain legs (lock here, mint there, then burn here)
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Escrows a sender's tokens that are leaving for another chain.
    ///         The messenger calls it only for the sender that called it. The
    ///         tokens stay in this ticker's supply, and so stay backed, until
    ///         their mint on the other chain is proven.
    function messengerLock(address from, uint256 amount) external onlyRole(MESSENGER_ROLE) {
        _requireLive();
        if (!canSend(from)) revert ERC7943CannotSend(from);
        uint256 a = availableBalanceOf(from);
        if (amount > a) revert InsufficientAvailable(from, amount, a);
        super._update(from, msg.sender, amount);
    }

    /// @notice Burns escrow whose mint on the other chain is proven.
    function messengerBurnLocked(uint256 amount) external onlyRole(MESSENGER_ROLE) {
        _requireLive();
        super._update(msg.sender, address(0), amount);
    }

    /// @notice Returns escrow of a cancelled move to an admitted holder.
    function messengerUnlock(address to, uint256 amount) external onlyRole(MESSENGER_ROLE) {
        _requireLive();
        if (!canReceive(to)) revert ERC7943CannotReceive(to);
        super._update(msg.sender, to, amount);
    }

    function _requireLive() internal view {
        if (retired) revert TokenRetired();
        _requireNotPaused();
    }

    /// @notice Mints tokens returning from another chain, on an attested
    ///         message. Their backing never left the omnibus.
    function messengerMint(address to, uint256 amount) external onlyRole(MESSENGER_ROLE) {
        _mint(to, amount);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                ERC-7943 surface
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The issuer's own holders, plus every member bank's registered
    ///         settlement wallet: banks hold each other's tokenized deposits.
    ///         Never a messenger: its escrow is not a holding.
    function canSend(address account) public view returns (bool) {
        return _isHolder(account);
    }

    function canReceive(address account) public view returns (bool) {
        return _isHolder(account);
    }

    function _isHolder(address account) internal view returns (bool) {
        return !blocked[account] && !hasRole(MESSENGER_ROLE, account)
            && (policy.isAuthorized(account) || LEDGER.memberOfWallet(account) != bytes32(0));
    }

    function canTransfer(address from, address to, uint256 amount) external view returns (bool) {
        return !retired && !paused() && canSend(from) && canReceive(to) && availableBalanceOf(from) >= amount;
    }

    function getFrozenTokens(address account) external view returns (uint256) {
        return _frozen[account];
    }

    function heldBalanceOf(address account) external view returns (uint256) {
        return _held[account];
    }

    function unfrozenBalanceOf(address account) public view returns (uint256) {
        uint256 bal = balanceOf(account);
        uint256 frz = _frozen[account];
        return bal > frz ? bal - frz : 0;
    }

    /// @notice What the holder can move or redeem now: not frozen, not held.
    function availableBalanceOf(address account) public view returns (uint256) {
        uint256 u = unfrozenBalanceOf(account);
        uint256 h = _held[account];
        return u > h ? u - h : 0;
    }

    function setFrozenTokens(address account, uint256 amount) external onlyRole(COMPLIANCE_ROLE) returns (bool) {
        if (account == address(0)) revert ZeroAddress();
        if (hasRole(MESSENGER_ROLE, account)) revert EscrowAccount(account);
        _frozen[account] = amount;
        emit Frozen(account, amount);
        return true;
    }

    /// @notice Regulatory transfer between two real accounts. It overrides
    ///         freezes, never holds: funds reserved for an in-flight payment
    ///         stay put until that payment settles, is rejected or expires.
    function forcedTransfer(address from, address to, uint256 amount)
        external
        onlyRole(COMPLIANCE_ROLE)
        whenNotPaused
        returns (bool)
    {
        if (retired) revert TokenRetired();
        if (from == address(0) || to == address(0)) revert ZeroAddress();
        if (from == to) revert SelfTransfer();
        if (hasRole(MESSENGER_ROLE, from)) revert EscrowAccount(from);
        if (!canReceive(to)) revert ERC7943CannotReceive(to);
        uint256 bal = balanceOf(from);
        uint256 h = _held[from];
        if (amount > bal - h) revert InsufficientAvailable(from, amount, bal - h);

        uint256 a = availableBalanceOf(from);
        if (amount > a) {
            _frozen[from] -= (amount - a);
            emit Frozen(from, _frozen[from]);
        }
        super._update(from, to, amount);
        emit ForcedTransfer(from, to, amount);
        return true;
    }

    /// @notice A holder lost its key: balance and freeze move to the
    ///         replacement, and the lost key is blocked for good, so later
    ///         inflows to it fail instead of reaching whoever has the key.
    function recover(address lost, address replacement)
        external
        onlyRole(COMPLIANCE_ROLE)
        whenNotPaused
        returns (uint256 amount)
    {
        if (retired) revert TokenRetired();
        if (lost == address(0) || replacement == address(0)) revert ZeroAddress();
        if (lost == replacement) revert SelfTransfer();
        if (hasRole(MESSENGER_ROLE, lost)) revert EscrowAccount(lost);
        if (!canReceive(replacement)) revert ERC7943CannotReceive(replacement);
        if (_held[lost] != 0) revert HoldsOutstanding(lost, _held[lost]);

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

    /*//////////////////////////////////////////////////////////////////////////
                                Replacement
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The ledger retires a replaced ticker, or an abandoned successor.
    function retire() external {
        if (msg.sender != address(LEDGER)) revert OnlyLedger(msg.sender);
        retired = true;
        emit Retired();
    }

    /// @notice As a successor, re-creates holders' balances, freezes and
    ///         blocks from the paused predecessor the ledger names. Anyone may
    ///         call it, in batches; the data comes from the predecessor itself,
    ///         and the ledger stops the total at the old supply and completes
    ///         the replacement only when every unit exists here.
    /// @dev Mints bypass pause and holder policy: they restore balances the
    ///      bank already issued, they do not issue new ones, and the ledger
    ///      does not encumber backing a second time.
    function migrateBalances(address[] calldata holders) external {
        BankToken pred = BankToken(LEDGER.predecessorOf(address(this)));
        for (uint256 i = 0; i < holders.length; i++) {
            address h = holders[i];
            if (migrated[h]) continue;
            migrated[h] = true;
            // Blocks and freezes carry over even with nothing to migrate: a lost
            // key recovered to a new one has a zero balance and must stay
            // blocked, or later inflows would reach whoever holds it.
            if (pred.blocked(h)) blocked[h] = true;
            uint256 f = pred.getFrozenTokens(h);
            if (f != 0) {
                _frozen[h] = f;
                emit Frozen(h, f);
            }
            uint256 bal = pred.balanceOf(h);
            if (bal != 0) {
                LEDGER.recordMigration(bal);
                super._update(address(0), h, bal);
            }
            emit BalanceMigrated(h, bal, f);
        }
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

    /// @dev Transfers need both sides admitted and an available balance;
    ///      mints need an admitted receiver; burns need an unfrozen balance.
    ///      forcedTransfer and recover call ERC20._update directly, which is
    ///      exactly the override they exist to perform.
    function _update(address from, address to, uint256 value) internal override {
        if (retired) revert TokenRetired();
        _requireNotPaused();
        if (from != address(0) && to != address(0)) {
            if (!canSend(from)) revert ERC7943CannotSend(from);
            if (!canReceive(to)) revert ERC7943CannotReceive(to);
            uint256 a = availableBalanceOf(from);
            if (value > a) revert ERC7943InsufficientUnfrozenBalance(from, value, a);
        } else if (from == address(0)) {
            if (!canReceive(to)) revert ERC7943CannotReceive(to);
        } else {
            uint256 u = unfrozenBalanceOf(from);
            if (value > u) revert ERC7943InsufficientUnfrozenBalance(from, value, u);
        }
        super._update(from, to, value);
    }

}
