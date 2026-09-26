// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { AccessControlDefaultAdminRules } from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";
import { IERC20Metadata } from "@openzeppelin/contracts/token/ERC20/extensions/IERC20Metadata.sol";

/// @dev What the ledger asks of a ticker it replaces, or replaces it with.
interface IReplaceable {

    function paused() external view returns (bool);
    function totalHeld() external view returns (uint256);
    function retire() external;

}

/**
 * @title OmnibusLedger
 * @notice The operator's register of member positions in the joint (omnibus)
 *         account it administers at the central bank.
 *
 * @dev THE MODEL. The reserves sit in ONE account at the Fed. This contract
 *      is the positions ledger over that account, the role a prefunded
 *      instant-payment system's ledger plays over its joint account: the Fed
 *      sees one balance, the operator tracks who owns which part of it, and a
 *      payment between two members moves ownership inside the account
 *      without touching Fedwire.
 *
 *      Each member bank issues its OWN ticker (dtA, dtB) against its
 *      position. A member's position splits into:
 *
 *        position = backing + pendingDefund + free
 *
 *      - backing        is encumbered by the member's outstanding tokens and
 *                       always equals its token's totalSupply;
 *      - pendingDefund  is on its way out over Fedwire;
 *      - free           can be minted against or defunded.
 *
 *      Backing can leave a member's position only by moving to ANOTHER
 *      member's position, together with the tokens it backs (a cross-bank
 *      payment through the PaymentRouter). It can never be defunded. So a
 *      holder of dtA is backed by reserves at the Fed that Bank A
 *      cannot withdraw while the token is outstanding.
 *
 *      TWO CLOCKS. Tokens move 24x7. Reserves move only when Fedwire is open.
 *      Funding and defunding are therefore asynchronous: the operator funding
 *      service posts a credit here when the Fed confirms one (camt.054), and
 *      a defund is requested here, executed over Fedwire, then confirmed.
 *
 *      RECONCILIATION. `omnibusTotal` is the balance the Fed statement must
 *      show. A separate reconciler role attests the Fed's figure; any
 *      difference halts minting and defunding until the books agree. The
 *      funding service cannot attest its own work.
 */
contract OmnibusLedger is AccessControlDefaultAdminRules {

    /*//////////////////////////////////////////////////////////////////////////
                                    Roles
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Admits, suspends and configures members. Intended to sit
    ///         behind a multisig and a timelock.
    bytes32 public constant GOVERNOR_ROLE = keccak256("GOVERNOR_ROLE");

    /// @notice Restrictive actions that must not wait for a timelock:
    ///         suspending a member. Undoing them (reinstate) is the governor's.
    bytes32 public constant GUARDIAN_ROLE = keccak256("GUARDIAN_ROLE");

    /// @notice The operator's funding service: posts Fedwire credits and defund
    ///         outcomes, and Fed interest. Cannot attest the Fed balance.
    bytes32 public constant FUNDING_ROLE = keccak256("FUNDING_ROLE");

    /// @notice Attests the Fed's statement balance. Held by a different key
    ///         (and team) than FUNDING_ROLE.
    bytes32 public constant RECONCILER_ROLE = keccak256("RECONCILER_ROLE");

    /// @notice The payment router: moves backing between members.
    bytes32 public constant ROUTER_ROLE = keccak256("ROUTER_ROLE");

    /// @notice The netting contract: moves free position between members,
    ///         one obligation at a time (gross) or as verified net positions.
    bytes32 public constant NETTING_ROLE = keccak256("NETTING_ROLE");

    /// @notice The cross-chain messenger: records tokens that left for, or
    ///         returned from, another chain. Backing stays here either way.
    bytes32 public constant MESSENGER_ROLE = keccak256("MESSENGER_ROLE");

    /// @notice Every ticker and every position uses 6 decimals: $1 = 1e6.
    uint8 public constant DECIMALS = 6;

    /// @notice One cent in ledger units. Fedwire moves whole cents, so a
    ///         defund must be a whole number of cents; positions themselves
    ///         may carry sub-cent amounts from interest and token payments.
    uint256 public constant CENT = 1e4;

    /*//////////////////////////////////////////////////////////////////////////
                                    State
    //////////////////////////////////////////////////////////////////////////*/

    struct Member {
        address token; // the member's ticker
        address operator; // the member's gateway key: requests defunds, answers payments
        address approver; // a second key that must approve each defund (maker-checker)
        bool admitted;
        bool suspended;
        bool requiresAcceptance; // inbound cross-bank payments wait for this member's accept
        uint256 position;
        uint256 backing;
        uint256 pendingDefund;
        uint256 positionSeconds; // time-weighted position since the last interest distribution
        uint64 lastTouch;
        uint256 issuanceCap; // most tokens this member may have outstanding; 0 = its position is the cap
        uint256 prefundRequirement; // free position that minting and defunding must leave behind
        uint256 remoteSupply; // its tokens currently on other chains, still backed here
    }

    /// @notice A member's ticker being replaced: its successor, the old supply
    ///         to re-create there, and how much has been re-created so far.
    struct Replacement {
        address successor;
        uint256 target;
        uint256 migrated;
    }

    enum DefundStatus {
        None,
        Requested,
        Approved,
        Completed,
        Failed,
        Cancelled
    }

    struct Defund {
        bytes32 memberId;
        uint256 amount;
        DefundStatus status;
    }

    mapping(bytes32 memberId => Member) internal _members;
    mapping(address token => bytes32 memberId) public memberOfToken;
    bytes32[] public memberIds;

    /// @notice The balance the Fed's statement for the joint account must
    ///         show: every position plus interest not yet allocated.
    uint256 public omnibusTotal;
    uint256 public undistributedInterest;

    /// @notice Fedwire references already posted (UETR / IMAD). A Fed credit
    ///         is posted once, however many times the notification arrives.
    mapping(bytes32 fedRef => bool) public fedRefUsed;

    mapping(bytes32 defundId => Defund) public defunds;

    /// @notice Member settlement wallets. A wallet listed here may hold and
    ///         move any member's ticker: banks hold each other's tokenized
    ///         deposits without each issuer admitting every other bank.
    mapping(address wallet => bytes32 memberId) public memberOfWallet;

    uint256 public lastFedBalance;
    bytes32 public lastStatementRef;
    uint64 public lastAttestedAt;
    bool public reconciliationBreak;
    int256 public breakAmount;

    /*//////////////////////////////////////////////////////////////////////////
                                    Events
    //////////////////////////////////////////////////////////////////////////*/

    event MemberAdmitted(
        bytes32 indexed memberId, address token, address operator, address approver, bool requiresAcceptance
    );
    event MemberSuspended(bytes32 indexed memberId);
    event MemberReinstated(bytes32 indexed memberId);
    event OperatorChanged(bytes32 indexed memberId, address operator);
    event ApproverChanged(bytes32 indexed memberId, address approver);
    event AcceptancePolicyChanged(bytes32 indexed memberId, bool requiresAcceptance);
    event Funded(bytes32 indexed memberId, uint256 amount, bytes32 indexed fedRef);
    event DefundRequested(bytes32 indexed defundId, bytes32 indexed memberId, uint256 amount);
    event DefundApproved(bytes32 indexed defundId, bytes32 indexed memberId, uint256 amount);
    event DefundCancelled(bytes32 indexed defundId, bytes32 indexed memberId, uint256 amount);
    event Defunded(bytes32 indexed defundId, bytes32 indexed memberId, uint256 amount, bytes32 fedRef);
    event DefundFailed(bytes32 indexed defundId, bytes32 indexed memberId, uint256 amount, bytes32 reason);
    event Encumbered(bytes32 indexed memberId, uint256 amount);
    event Released(bytes32 indexed memberId, uint256 amount);
    event BackingMoved(bytes32 indexed fromMember, bytes32 indexed toMember, uint256 amount);
    event HeldTokensSettled(bytes32 indexed issuer, bytes32 indexed holder, uint256 amount);
    event FreePositionMoved(bytes32 indexed fromMember, bytes32 indexed toMember, uint256 amount);
    event NetPositionsApplied(uint256 members, uint256 netMoved);
    event WalletRegistered(bytes32 indexed memberId, address wallet);
    event WalletUnregistered(bytes32 indexed memberId, address wallet);
    event LimitsSet(bytes32 indexed memberId, uint256 issuanceCap, uint256 prefundRequirement);
    event RemoteSupplyChanged(bytes32 indexed memberId, int256 delta, uint256 remoteSupply);
    event InterestDistributed(bytes32 indexed fedRef, uint256 amount, uint256 carried);
    event InterestAllocated(bytes32 indexed memberId, uint256 amount);
    event Reconciled(uint256 fedBalance, bytes32 statementRef);
    event ReconciliationBreak(uint256 fedBalance, uint256 omnibusTotal, int256 difference, bytes32 statementRef);
    event BreakCleared(bytes32 reason);
    event TokenReplacementBegun(bytes32 indexed memberId, address indexed oldToken, address indexed successor, uint256 supply);
    event BalancesMigrated(bytes32 indexed memberId, uint256 amount, uint256 migrated);
    event TokenReplaced(bytes32 indexed memberId, address indexed oldToken, address indexed successor);
    event TokenReplacementCancelled(bytes32 indexed memberId, address indexed successor);

    /*//////////////////////////////////////////////////////////////////////////
                                    Errors
    //////////////////////////////////////////////////////////////////////////*/

    error UnknownMember(bytes32 memberId);
    error AlreadyAdmitted(bytes32 memberId);
    error MemberSuspendedError(bytes32 memberId);
    error NotTheOperator(bytes32 memberId, address caller);
    error NotARegisteredToken(address caller);
    error WrongDecimals(address token, uint8 decimals);
    error TokenAlreadyRegistered(address token);
    error ZeroAddress();
    error ZeroAmount();
    error FedRefAlreadyUsed(bytes32 fedRef);
    error InsufficientFreePosition(bytes32 memberId, uint256 requested, uint256 free);
    error BackingUnderflow(bytes32 memberId, uint256 requested, uint256 backing);
    error DuplicateDefund(bytes32 defundId);
    error DefundNotPending(bytes32 defundId);
    error DefundNotRequested(bytes32 defundId);
    error NotTheApprover(bytes32 memberId, address caller);
    error ApproverMustDiffer(address key);
    error SameMember(bytes32 memberId);
    error ReconciliationHalt(int256 breakAmount);
    error NotWholeCents(uint256 amount);
    error IssuanceCapExceeded(bytes32 memberId, uint256 wouldBe, uint256 cap);
    error BelowPrefundRequirement(bytes32 memberId, uint256 freeAfter, uint256 requirement);
    error WalletAlreadyRegistered(address wallet, bytes32 memberId);
    error NetsDoNotBalance(int256 sum);
    error LengthMismatch();
    error RemoteSupplyUnderflow(bytes32 memberId, uint256 requested, uint256 remote);
    error ReplacementInProgress(bytes32 memberId);
    error NoReplacement(bytes32 memberId);
    error NotASuccessor(address token);
    error TokenNotPaused(address token);
    error HoldsOutstanding(address token, uint256 held);
    error SuccessorNotEmpty(address token, uint256 supply);
    error MigrationExceeds(bytes32 memberId, uint256 wouldBe, uint256 target);
    error MigrationIncomplete(bytes32 memberId, uint256 migrated, uint256 target);
    error SupplyChanged(address token, uint256 supply, uint256 target);

    /// @param admin in production, a TimelockController; handing the role
    ///        over is two-step and waits `adminDelay`.
    constructor(address admin, uint48 adminDelay) AccessControlDefaultAdminRules(adminDelay, admin) { }

    /*//////////////////////////////////////////////////////////////////////////
                                Membership
    //////////////////////////////////////////////////////////////////////////*/

    function admitMember(
        bytes32 memberId,
        address token,
        address operator,
        address approver,
        bool requiresAcceptance
    ) external onlyRole(GOVERNOR_ROLE) {
        if (token == address(0) || operator == address(0) || approver == address(0)) revert ZeroAddress();
        if (approver == operator) revert ApproverMustDiffer(approver);
        if (memberId == bytes32(0)) revert UnknownMember(memberId); // zero means "not a member" in memberOfToken
        if (_members[memberId].admitted) revert AlreadyAdmitted(memberId);
        if (memberOfToken[token] != bytes32(0)) revert TokenAlreadyRegistered(token);
        uint8 d = IERC20Metadata(token).decimals();
        if (d != DECIMALS) revert WrongDecimals(token, d);

        Member storage m = _members[memberId];
        m.token = token;
        m.operator = operator;
        m.approver = approver;
        m.admitted = true;
        m.requiresAcceptance = requiresAcceptance;
        m.lastTouch = uint64(block.timestamp);
        memberOfToken[token] = memberId;
        memberIds.push(memberId);
        emit MemberAdmitted(memberId, token, operator, approver, requiresAcceptance);
    }

    /// @notice Suspension stops a member minting, defunding and receiving
    ///         cross-bank payments. It does NOT stop its token holders
    ///         paying out to other members: their backing is at the Fed, and
    ///         moving it to a healthy member is how they exit at par.
    ///         The governor (behind a timelock) or the guardian (at once) may
    ///         suspend; only the governor reinstates.
    function suspend(bytes32 memberId) external {
        if (!hasRole(GUARDIAN_ROLE, msg.sender) && !hasRole(GOVERNOR_ROLE, msg.sender)) {
            revert AccessControlUnauthorizedAccount(msg.sender, GUARDIAN_ROLE);
        }
        _admitted(memberId).suspended = true;
        emit MemberSuspended(memberId);
    }

    function reinstate(bytes32 memberId) external onlyRole(GOVERNOR_ROLE) {
        _admitted(memberId).suspended = false;
        emit MemberReinstated(memberId);
    }

    function setOperator(bytes32 memberId, address operator) external onlyRole(GOVERNOR_ROLE) {
        if (operator == address(0)) revert ZeroAddress();
        Member storage m = _admitted(memberId);
        if (operator == m.approver) revert ApproverMustDiffer(operator);
        m.operator = operator;
        emit OperatorChanged(memberId, operator);
    }

    function setApprover(bytes32 memberId, address approver) external onlyRole(GOVERNOR_ROLE) {
        if (approver == address(0)) revert ZeroAddress();
        Member storage m = _admitted(memberId);
        if (approver == m.operator) revert ApproverMustDiffer(approver);
        m.approver = approver;
        emit ApproverChanged(memberId, approver);
    }

    /// @notice A member chooses whether inbound cross-bank payments wait for
    ///         its acceptance (the RTP pacs.002 step) or settle at once.
    function setRequiresAcceptance(bytes32 memberId, bool required) external {
        Member storage m = _admitted(memberId);
        if (msg.sender != m.operator) revert NotTheOperator(memberId, msg.sender);
        m.requiresAcceptance = required;
        emit AcceptancePolicyChanged(memberId, required);
    }

    /// @notice The operator lists a member's settlement wallet. Governance, not the
    ///         member, does it: a listed wallet may hold every issuer's
    ///         ticker, so listing a customer's wallet would bypass the other
    ///         issuers' onboarding.
    function registerWallet(bytes32 memberId, address wallet) external onlyRole(GOVERNOR_ROLE) {
        if (wallet == address(0)) revert ZeroAddress();
        _admitted(memberId);
        if (memberOfWallet[wallet] != bytes32(0)) revert WalletAlreadyRegistered(wallet, memberOfWallet[wallet]);
        memberOfWallet[wallet] = memberId;
        emit WalletRegistered(memberId, wallet);
    }

    function unregisterWallet(address wallet) external onlyRole(GOVERNOR_ROLE) {
        bytes32 id = memberOfWallet[wallet];
        delete memberOfWallet[wallet];
        emit WalletUnregistered(id, wallet);
    }

    /// @notice Funding controls. A cap limits a member's outstanding tokens
    ///         below its position; a prefund requirement keeps a floor of
    ///         free position for settling obligations, as RTP's Prefunded
    ///         Requirement does.
    function setLimits(bytes32 memberId, uint256 issuanceCap, uint256 prefundRequirement)
        external
        onlyRole(GOVERNOR_ROLE)
    {
        Member storage m = _admitted(memberId);
        m.issuanceCap = issuanceCap;
        m.prefundRequirement = prefundRequirement;
        emit LimitsSet(memberId, issuanceCap, prefundRequirement);
    }

    /*//////////////////////////////////////////////////////////////////////////
                        Funding and defunding over Fedwire
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Posts a Fedwire credit to the joint account (camt.054) to the
    ///         member it was sent for. Idempotent on the Fedwire reference.
    function creditFunding(bytes32 memberId, uint256 amount, bytes32 fedRef) external onlyRole(FUNDING_ROLE) {
        if (amount == 0) revert ZeroAmount();
        if (fedRefUsed[fedRef]) revert FedRefAlreadyUsed(fedRef);
        Member storage m = _admitted(memberId);
        fedRefUsed[fedRef] = true;
        _touch(m);
        m.position += amount;
        omnibusTotal += amount;
        emit Funded(memberId, amount, fedRef);
    }

    /// @notice The member's operator asks for reserves back to its master
    ///         account. Only free position can leave; backing never can. The
    ///         amount is reserved at once, and nothing goes to Fedwire until
    ///         the member's approver signs off, as an RTP disbursement needs
    ///         a second authorized user to approve it.
    function requestDefund(bytes32 memberId, uint256 amount, bytes32 clientRef) external returns (bytes32 defundId) {
        Member storage m = _admitted(memberId);
        if (msg.sender != m.operator) revert NotTheOperator(memberId, msg.sender);
        if (m.suspended) revert MemberSuspendedError(memberId);
        if (reconciliationBreak) revert ReconciliationHalt(breakAmount);
        if (amount == 0) revert ZeroAmount();
        if (amount % CENT != 0) revert NotWholeCents(amount);
        uint256 f = _free(m);
        if (amount > f) revert InsufficientFreePosition(memberId, amount, f);
        if (f - amount < m.prefundRequirement) revert BelowPrefundRequirement(memberId, f - amount, m.prefundRequirement);

        defundId = keccak256(abi.encode(memberId, clientRef));
        if (defunds[defundId].status != DefundStatus.None) revert DuplicateDefund(defundId);
        defunds[defundId] = Defund({ memberId: memberId, amount: amount, status: DefundStatus.Requested });
        m.pendingDefund += amount;
        emit DefundRequested(defundId, memberId, amount);
    }

    /// @notice The member's second key approves a requested defund; the operator's
    ///         funding service executes only approved ones.
    function approveDefund(bytes32 defundId) external {
        Defund storage d = defunds[defundId];
        if (d.status != DefundStatus.Requested) revert DefundNotRequested(defundId);
        Member storage m = _members[d.memberId];
        if (msg.sender != m.approver) revert NotTheApprover(d.memberId, msg.sender);
        if (m.suspended) revert MemberSuspendedError(d.memberId);
        if (reconciliationBreak) revert ReconciliationHalt(breakAmount);
        d.status = DefundStatus.Approved;
        emit DefundApproved(defundId, d.memberId, d.amount);
    }

    /// @notice Either member key withdraws a request not yet approved.
    function cancelDefund(bytes32 defundId) external {
        Defund storage d = defunds[defundId];
        if (d.status != DefundStatus.Requested) revert DefundNotRequested(defundId);
        Member storage m = _members[d.memberId];
        if (msg.sender != m.operator && msg.sender != m.approver) revert NotTheOperator(d.memberId, msg.sender);
        d.status = DefundStatus.Cancelled;
        m.pendingDefund -= d.amount;
        emit DefundCancelled(defundId, d.memberId, d.amount);
    }

    /// @notice The Fed confirmed the outbound Fedwire.
    function confirmDefund(bytes32 defundId, bytes32 fedRef) external onlyRole(FUNDING_ROLE) {
        Defund storage d = defunds[defundId];
        if (d.status != DefundStatus.Approved) revert DefundNotPending(defundId);
        if (fedRefUsed[fedRef]) revert FedRefAlreadyUsed(fedRef);
        fedRefUsed[fedRef] = true;
        Member storage m = _members[d.memberId];
        _touch(m);
        d.status = DefundStatus.Completed;
        m.pendingDefund -= d.amount;
        m.position -= d.amount;
        omnibusTotal -= d.amount;
        emit Defunded(defundId, d.memberId, d.amount, fedRef);
    }

    /// @notice The outbound Fedwire was rejected or never sent; the amount
    ///         returns to the member's free position.
    function failDefund(bytes32 defundId, bytes32 reason) external onlyRole(FUNDING_ROLE) {
        Defund storage d = defunds[defundId];
        if (d.status != DefundStatus.Approved) revert DefundNotPending(defundId);
        d.status = DefundStatus.Failed;
        _members[d.memberId].pendingDefund -= d.amount;
        emit DefundFailed(defundId, d.memberId, d.amount, reason);
    }

    /*//////////////////////////////////////////////////////////////////////////
                    Backing: called by tickers and the router only
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice A ticker mints: encumber the member's free position.
    function encumber(uint256 amount) external {
        bytes32 id = _callerMember();
        Member storage m = _members[id];
        if (m.suspended) revert MemberSuspendedError(id);
        if (reconciliationBreak) revert ReconciliationHalt(breakAmount);
        uint256 f = _free(m);
        if (amount > f) revert InsufficientFreePosition(id, amount, f);
        if (f - amount < m.prefundRequirement) revert BelowPrefundRequirement(id, f - amount, m.prefundRequirement);
        if (m.issuanceCap != 0 && m.backing + amount > m.issuanceCap) {
            revert IssuanceCapExceeded(id, m.backing + amount, m.issuanceCap);
        }
        m.backing += amount;
        emit Encumbered(id, amount);
    }

    /// @notice A ticker burns on redemption: its backing becomes free again.
    function release(uint256 amount) external {
        bytes32 id = _callerMember();
        Member storage m = _members[id];
        if (amount + m.remoteSupply > m.backing) revert BackingUnderflow(id, amount, m.backing - m.remoteSupply);
        m.backing -= amount;
        emit Released(id, amount);
    }

    /// @notice A cross-bank payment: the sending member's backing and the
    ///         reserves under it become the receiving member's. The Fed
    ///         balance does not change; ownership inside it does.
    function moveBacking(bytes32 fromMember, bytes32 toMember, uint256 amount) external onlyRole(ROUTER_ROLE) {
        if (fromMember == toMember) revert SameMember(fromMember);
        Member storage f = _admitted(fromMember);
        Member storage t = _admitted(toMember);
        if (t.suspended) revert MemberSuspendedError(toMember);
        if (amount + f.remoteSupply > f.backing) revert BackingUnderflow(fromMember, amount, f.backing - f.remoteSupply);
        _touch(f);
        _touch(t);
        f.backing -= amount;
        f.position -= amount;
        t.position += amount;
        t.backing += amount;
        emit BackingMoved(fromMember, toMember, amount);
    }

    /// @notice A member presents another member's tokens it holds: they are
    ///         burned, and the issuer's backing under them becomes the
    ///         presenter's free position. Gross interbank settlement of a
    ///         held claim; the Fed balance does not change.
    function settleHeldTokens(bytes32 issuer, bytes32 holder, uint256 amount) external onlyRole(ROUTER_ROLE) {
        if (issuer == holder) revert SameMember(issuer);
        Member storage f = _admitted(issuer);
        Member storage t = _admitted(holder);
        if (amount + f.remoteSupply > f.backing) revert BackingUnderflow(issuer, amount, f.backing - f.remoteSupply);
        _touch(f);
        _touch(t);
        f.backing -= amount;
        f.position -= amount;
        t.position += amount;
        emit HeldTokensSettled(issuer, holder, amount);
    }

    /// @notice Gross settlement of one interbank obligation: free position
    ///         moves from payer to payee now. Backing is never touched.
    function transferFree(bytes32 fromMember, bytes32 toMember, uint256 amount) external onlyRole(NETTING_ROLE) {
        if (fromMember == toMember) revert SameMember(fromMember);
        Member storage f = _admitted(fromMember);
        Member storage t = _admitted(toMember);
        if (t.suspended) revert MemberSuspendedError(toMember);
        uint256 fr = _free(f);
        if (amount > fr) revert InsufficientFreePosition(fromMember, amount, fr);
        _touch(f);
        _touch(t);
        f.position -= amount;
        t.position += amount;
        emit FreePositionMoved(fromMember, toMember, amount);
    }

    /// @notice Multilateral net settlement: applies verified net positions
    ///         that sum to zero. Every net payer must cover its debit from
    ///         free position, so a cycle can never create credit.
    function applyNet(bytes32[] calldata members, int256[] calldata nets) external onlyRole(NETTING_ROLE) {
        if (members.length != nets.length) revert LengthMismatch();
        int256 sum;
        uint256 moved;
        for (uint256 i = 0; i < members.length; i++) {
            sum += nets[i];
        }
        if (sum != 0) revert NetsDoNotBalance(sum);
        for (uint256 i = 0; i < members.length; i++) {
            Member storage m = _admitted(members[i]);
            _touch(m);
            if (nets[i] < 0) {
                uint256 debit = uint256(-nets[i]);
                uint256 fr = _free(m);
                if (debit > fr) revert InsufficientFreePosition(members[i], debit, fr);
                m.position -= debit;
                moved += debit;
            } else if (nets[i] > 0) {
                if (m.suspended) revert MemberSuspendedError(members[i]);
                m.position += uint256(nets[i]);
            }
        }
        emit NetPositionsApplied(members.length, moved);
    }

    /// @notice The messenger recorded tokens leaving for another chain
    ///         (burned here, minted there) or returning (burned there,
    ///         minted here). Backing stays in the omnibus throughout, so
    ///         backing == local supply + remote supply.
    function recordRemote(bytes32 memberId, int256 delta) external onlyRole(MESSENGER_ROLE) {
        Member storage m = _admitted(memberId);
        if (delta >= 0) {
            m.remoteSupply += uint256(delta);
        } else {
            uint256 d = uint256(-delta);
            if (d > m.remoteSupply) revert RemoteSupplyUnderflow(memberId, d, m.remoteSupply);
            m.remoteSupply -= d;
        }
        emit RemoteSupplyChanged(memberId, delta, m.remoteSupply);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Interest
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Allocates interest the Fed credited to the joint account, by
    ///         each member's time-weighted position since the last
    ///         distribution. A position held for a second earns a second's
    ///         share, so topping up just before a distribution buys nothing.
    ///         Interest goes to member positions, not to token holders.
    function distributeInterest(uint256 amount, bytes32 fedRef) external onlyRole(FUNDING_ROLE) {
        if (amount == 0) revert ZeroAmount();
        if (fedRefUsed[fedRef]) revert FedRefAlreadyUsed(fedRef);
        fedRefUsed[fedRef] = true;
        omnibusTotal += amount;

        uint256 pot = amount + undistributedInterest;
        uint256 total;
        uint256 n = memberIds.length;
        for (uint256 i = 0; i < n; i++) {
            Member storage m = _members[memberIds[i]];
            _touch(m);
            total += m.positionSeconds;
        }
        if (total == 0) {
            undistributedInterest = pot;
            emit InterestDistributed(fedRef, amount, pot);
            return;
        }
        uint256 paid;
        for (uint256 i = 0; i < n; i++) {
            Member storage m = _members[memberIds[i]];
            uint256 share = (pot * m.positionSeconds) / total;
            m.positionSeconds = 0;
            if (share > 0) {
                m.position += share;
                paid += share;
                emit InterestAllocated(memberIds[i], share);
            }
        }
        undistributedInterest = pot - paid;
        emit InterestDistributed(fedRef, amount, undistributedInterest);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                Reconciliation
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Records the Fed's statement balance for the joint account
    ///         (camt.053 closing balance, taken when no Fedwire is in flight).
    ///         A mismatch halts minting and defunding; a later matching
    ///         statement clears it.
    function attestFedBalance(uint256 fedBalance, bytes32 statementRef) external onlyRole(RECONCILER_ROLE) {
        lastFedBalance = fedBalance;
        lastStatementRef = statementRef;
        lastAttestedAt = uint64(block.timestamp);
        int256 diff = int256(fedBalance) - int256(omnibusTotal);
        if (diff == 0) {
            if (reconciliationBreak) {
                reconciliationBreak = false;
                breakAmount = 0;
                emit BreakCleared(statementRef);
            }
            emit Reconciled(fedBalance, statementRef);
        } else {
            reconciliationBreak = true;
            breakAmount = diff;
            emit ReconciliationBreak(fedBalance, omnibusTotal, diff, statementRef);
        }
    }

    function clearBreak(bytes32 reason) external onlyRole(GOVERNOR_ROLE) {
        reconciliationBreak = false;
        breakAmount = 0;
        emit BreakCleared(reason);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Views
    //////////////////////////////////////////////////////////////////////////*/

    function member(bytes32 memberId) external view returns (Member memory) {
        return _members[memberId];
    }

    function freePosition(bytes32 memberId) external view returns (uint256) {
        return _free(_members[memberId]);
    }

    /// @notice How much more the member may issue now: free position above
    ///         its prefund requirement, and within its cap.
    function mintCapacity(bytes32 memberId) external view returns (uint256) {
        Member storage m = _members[memberId];
        uint256 f = _free(m);
        uint256 cap = f > m.prefundRequirement ? f - m.prefundRequirement : 0;
        if (m.issuanceCap != 0) {
            uint256 room = m.issuanceCap > m.backing ? m.issuanceCap - m.backing : 0;
            if (room < cap) cap = room;
        }
        return cap;
    }

    function memberCount() external view returns (uint256) {
        return memberIds.length;
    }

    /// @notice The ledger's own invariants, for monitors and tests:
    ///         Σ position + undistributed interest == omnibusTotal, and for
    ///         every member backing == token supply here + on other chains,
    ///         and backing + pendingDefund <= position.
    /*//////////////////////////////////////////////////////////////////////////
                            Replacing a member's ticker
    //////////////////////////////////////////////////////////////////////////*/

    mapping(bytes32 memberId => Replacement) public replacements;
    mapping(address successor => bytes32 memberId) public replacing;

    /// @notice Starts replacing a member's ticker with `successor` (a fixed
    ///         contract). Both must be paused, the old one with no payment
    ///         holding its tokens, the successor with no supply. Its backing
    ///         does not move: the old supply is re-created on the successor,
    ///         holder by holder, and the old ticker is retired at completion.
    /// @dev Governor only, so behind the timelock. Inbound payments to the
    ///      member should be answered or expired first: accepting one mints
    ///      the member's ticker, which is paused.
    function beginTokenReplacement(bytes32 memberId, address successor) external onlyRole(GOVERNOR_ROLE) {
        Member storage m = _admitted(memberId);
        if (replacements[memberId].successor != address(0)) revert ReplacementInProgress(memberId);
        if (successor == address(0)) revert ZeroAddress();
        if (memberOfToken[successor] != bytes32(0) || replacing[successor] != bytes32(0)) {
            revert TokenAlreadyRegistered(successor);
        }
        uint8 d = IERC20Metadata(successor).decimals();
        if (d != DECIMALS) revert WrongDecimals(successor, d);
        uint256 s = IERC20Metadata(successor).totalSupply();
        if (s != 0) revert SuccessorNotEmpty(successor, s);
        if (!IReplaceable(m.token).paused()) revert TokenNotPaused(m.token);
        if (!IReplaceable(successor).paused()) revert TokenNotPaused(successor);
        uint256 held = IReplaceable(m.token).totalHeld();
        if (held != 0) revert HoldsOutstanding(m.token, held);

        uint256 target = IERC20Metadata(m.token).totalSupply();
        replacements[memberId] = Replacement({ successor: successor, target: target, migrated: 0 });
        replacing[successor] = memberId;
        emit TokenReplacementBegun(memberId, m.token, successor, target);
    }

    /// @notice The ticker a successor re-creates balances from.
    function predecessorOf(address successor) external view returns (address) {
        bytes32 id = replacing[successor];
        if (id == bytes32(0)) revert NotASuccessor(successor);
        return _members[id].token;
    }

    /// @notice Called by the successor for each balance it re-creates; the
    ///         total can never exceed the old supply.
    function recordMigration(uint256 amount) external {
        bytes32 id = replacing[msg.sender];
        if (id == bytes32(0)) revert NotASuccessor(msg.sender);
        Replacement storage r = replacements[id];
        uint256 wouldBe = r.migrated + amount;
        if (wouldBe > r.target) revert MigrationExceeds(id, wouldBe, r.target);
        r.migrated = wouldBe;
        emit BalancesMigrated(id, amount, wouldBe);
    }

    /// @notice Anyone completes a replacement once every unit of the old
    ///         supply exists on the successor: the member's ticker becomes the
    ///         successor and the old one is retired for good.
    function completeTokenReplacement(bytes32 memberId) external {
        Replacement memory r = replacements[memberId];
        if (r.successor == address(0)) revert NoReplacement(memberId);
        if (r.migrated != r.target) revert MigrationIncomplete(memberId, r.migrated, r.target);
        Member storage m = _members[memberId];
        address old = m.token;
        uint256 s = IERC20Metadata(old).totalSupply();
        if (s != r.target) revert SupplyChanged(old, s, r.target);

        delete memberOfToken[old];
        memberOfToken[r.successor] = memberId;
        m.token = r.successor;
        delete replacing[r.successor];
        delete replacements[memberId];
        IReplaceable(old).retire();
        emit TokenReplaced(memberId, old, r.successor);
    }

    /// @notice Abandons a replacement; the successor is retired, so balances
    ///         re-created on it can never move.
    function cancelTokenReplacement(bytes32 memberId) external onlyRole(GOVERNOR_ROLE) {
        Replacement memory r = replacements[memberId];
        if (r.successor == address(0)) revert NoReplacement(memberId);
        delete replacing[r.successor];
        delete replacements[memberId];
        IReplaceable(r.successor).retire();
        emit TokenReplacementCancelled(memberId, r.successor);
    }

    function invariantsHold() external view returns (bool) {
        uint256 sum = undistributedInterest;
        uint256 n = memberIds.length;
        for (uint256 i = 0; i < n; i++) {
            Member storage m = _members[memberIds[i]];
            sum += m.position;
            if (m.backing != IERC20Metadata(m.token).totalSupply() + m.remoteSupply) return false;
            if (m.backing + m.pendingDefund > m.position) return false;
        }
        return sum == omnibusTotal;
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Internals
    //////////////////////////////////////////////////////////////////////////*/

    function _admitted(bytes32 memberId) internal view returns (Member storage m) {
        m = _members[memberId];
        if (!m.admitted) revert UnknownMember(memberId);
    }

    function _callerMember() internal view returns (bytes32 id) {
        id = memberOfToken[msg.sender];
        if (id == bytes32(0)) revert NotARegisteredToken(msg.sender);
    }

    function _free(Member storage m) internal view returns (uint256) {
        uint256 used = m.backing + m.pendingDefund;
        return m.position > used ? m.position - used : 0;
    }

    function _touch(Member storage m) internal {
        uint64 nowTs = uint64(block.timestamp);
        if (nowTs > m.lastTouch) {
            m.positionSeconds += m.position * (nowTs - m.lastTouch);
        }
        m.lastTouch = nowTs;
    }

}
