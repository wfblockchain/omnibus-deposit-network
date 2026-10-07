// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { AccessControlDefaultAdminRules } from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";
import { IERC20 } from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { ECDSA } from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import { MessageHashUtils } from "@openzeppelin/contracts/utils/cryptography/MessageHashUtils.sol";
import { OmnibusLedger } from "../OmnibusLedger.sol";

import { IBurnMintToken } from "./IBurnMintToken.sol";
import { IRevocable } from "../HolderRegistry.sol";

/**
 * @title CrossChainMessenger
 * @notice Moves a member's tokenized deposits between chains by native
 *         issuance: the destination mints the bank's own token on an attested
 *         message, and the source burns its tokens once that mint is proven.
 *         Nothing is wrapped: a token on another chain is the same deposit,
 *         issued there, not a claim on tokens held somewhere else.
 *
 * @dev THE BACKING NEVER MOVES. Reserves stay in the operator's joint account at the
 *      Fed, recorded by the OmnibusLedger on the home chain. Home counts a
 *      member's supply on other chains as its remote supply; the ledger's
 *      invariant is backing == home supply + remote supply. Home supply
 *      includes tokens this messenger holds in escrow, so tokens are never
 *      under-backed while a move is in flight.
 *
 *      LOCK, MINT, THEN BURN. No token is burned before its mint is proven.
 *        1. `depositForBurn` moves the sender's tokens into this messenger's
 *           escrow and records the move PENDING, with a deadline.
 *        2. The destination mints, with both keys and inside its caps and
 *           rate limit, before the deadline. It records the nonce MINTED and
 *           sends an attested MINT_ACK back.
 *        3. On the MINT_ACK the source burns the escrow: the move is COMPLETED.
 *      If the destination cannot mint (halted, over a limit, attesters or
 *      issuer refusing, a recipient who cannot hold the token), anyone may
 *      `cancel` the move there once its deadline has passed, or at once when
 *      the recipient cannot hold the token. The destination records the
 *      nonce CANCELLED, so it can never mint it later, and sends an attested
 *      MINT_CANCEL; on it the source returns the escrow to `returnTo`, or to
 *      the issuing bank's suspense wallet when `returnTo` cannot hold the
 *      token. A nonce is MINTED or CANCELLED on the destination, never both,
 *      and a move is COMPLETED or CANCELLED on the source, never both.
 *
 *      COUNTED ONCE, WHEN MINTED. Home's `outstanding` and the ledger's remote
 *      supply count a move out of home only when its MINT_ACK arrives; until
 *      then the amount is home supply held in escrow, reserved against the
 *      corridor cap as `pendingOut`. A move into home counts off both at the
 *      moment home mints. So home never counts a token twice, and never counts
 *      as abroad a token that was never minted there.
 *
 *      ATTESTATION. A message is valid on the destination only with
 *      signatures from `threshold` distinct attesters (the operator's attestation
 *      service), sorted by signer address so a signature cannot be counted
 *      twice. The destination also checks that the message came from the
 *      messenger registered for its source domain, and that its nonce has
 *      not been used. On other chains LEDGER is the zero address.
 *
 *      HUB AND SPOKE. Tokens travel only between home and one other chain,
 *      never directly between two others. Home therefore knows exactly how
 *      much of each member's supply sits on each chain: `outstanding`, capped
 *      per corridor by `corridorCap`. A chain can never send home more than
 *      was sent to it, so a compromised chain (or a compromised attester set
 *      acting on its behalf) is contained to that corridor's cap. A corridor
 *      is closed until governance opens it.
 *
 *      DELIVERY WITH INSTRUCTIONS. A message may name a `destinationCaller`,
 *      the only address allowed to deliver it, and carry `hookData` for that
 *      caller. A settlement contract uses the pair to receive cash and act on
 *      it in the same transaction: it delivers the message itself, the tokens
 *      mint to it, and it reads the instruction from what `receiveMessage`
 *      returns. Nobody else can deliver the message first and leave the
 *      tokens unaccounted for.
 *
 *      TWO KEYS FOR EVERY MESSAGE. A token is its issuing bank's liability, so
 *      no one but that bank should be able to create it anywhere. A message
 *      acts only with the operator's attester threshold AND a signature from the
 *      issuing bank's own attestation key, set per member on each chain. A
 *      compromised operator attester set cannot mint a bank's deposits, and a
 *      compromised bank key cannot mint without the operator. A key can never hold
 *      both roles.
 *
 *      CAPS ON BOTH SIDES. Home caps each corridor; every other chain caps
 *      each member's supply there. Even if both key sets were compromised, a
 *      forged mint on another chain stops at that chain's cap, and home
 *      refuses to take back more than it sent.
 *
 *      A RATE LIMIT ON EVERY RECEIVING CHAIN, per source chain and member: a
 *      token bucket of `capacity` that refills linearly over `window`. Every
 *      mint here, and every escrow released here by a MINT_CANCEL, draws on
 *      it. A message over the bucket reverts and stays deliverable: it is
 *      retried once the bucket has refilled, or cancelled after its deadline.
 *      An unset bucket is closed. The bucket bounds how fast value can appear
 *      on this chain even when every signature on a forged message is valid,
 *      which an outbound limit on the source cannot do.
 *
 *      GOVERNANCE. Configuration (attesters, threshold, issuer keys, remotes,
 *      tokens, caps, rate limits, the move timeout) belongs to the default
 *      admin, which in production is a TimelockController: every change is
 *      visible for the timelock's delay before it takes effect. Handing the
 *      admin role over is itself two-step and delayed. Pausing is a separate
 *      role and takes effect at once.
 *
 *      FINALITY is the attesters' duty, not something a destination can
 *      check: they sign a message only once the event that produced it (a
 *      lock, a mint, a cancellation) is final where it happened.
 *
 *      PAUSE. Stops sending and delivery on this chain; escrow stays put and
 *      delivery resumes where it stopped. `cancel` still works while paused:
 *      it only closes a nonce here. The pauser is also the guardian: it may
 *      LOWER a corridor cap, a supply cap or a rate limit at once. Raising
 *      one is configuration, behind the timelock.
 *
 *      REVOCATIONS FOLLOW THE TOKEN, ADMISSIONS DO NOT. When a bank drops a
 *      holder, its registrar at home broadcasts the revocation as an attested
 *      POLICY message; on arrival the messenger revokes the holder in that
 *      chain's registry, with no delay. Admitting a holder stays a local
 *      decision on each chain. So the hub can only restrict, never admit: a
 *      compromised hub key cannot let anyone in.
 *
 *      THRESHOLD. The attester threshold is always a strict majority of the
 *      enabled attesters and never more than their number: a minority of
 *      compromised keys can never mint, and removing attesters can never make
 *      delivery impossible.
 */
contract CrossChainMessenger is AccessControlDefaultAdminRules, Pausable {

    /// @notice Envelope version 4: lock-then-burn. Adds `deadline` and
    ///         `refNonce`; kinds MINT_ACK and MINT_CANCEL replace v3's RETURN.
    uint8 public constant VERSION = 4;

    uint8 public constant KIND_TRANSFER = 0;
    // Kind 1 was RETURN in version 3; it no longer exists.
    uint8 public constant KIND_POLICY = 2;
    uint8 public constant KIND_MINT_ACK = 3;
    uint8 public constant KIND_MINT_CANCEL = 4;

    bytes32 private constant REGISTRAR_ROLE = keccak256("REGISTRAR_ROLE");

    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");

    /// @notice Bounds on how long a move may wait for its mint.
    uint64 public constant MIN_MOVE_TIMEOUT = 10 minutes;
    uint64 public constant MAX_MOVE_TIMEOUT = 30 days;

    /// @notice The attested message. Encoded with `abi.encode(envelope)`.
    /// @dev For a TRANSFER, `deadline` is the last moment (exclusive) the
    ///      destination may mint it. For a MINT_ACK or MINT_CANCEL, `refNonce`
    ///      is the TRANSFER's nonce on the chain that receives the reply, and
    ///      the other fields repeat the TRANSFER's.
    struct Envelope {
        uint8 version;
        uint8 kind;
        uint32 sourceDomain;
        uint32 destDomain;
        uint64 nonce;
        address sourceMessenger;
        bytes32 memberId;
        address sender;
        address recipient;
        uint256 amount;
        address destinationCaller;
        address returnTo;
        uint64 deadline;
        uint64 refNonce;
        bytes hookData;
    }

    /// @notice What `receiveMessage` delivered, for a destination caller
    ///         that acts on it. For a MINT_ACK nothing is delivered
    ///         (`recipient` is zero); for a MINT_CANCEL, `recipient` is where
    ///         the escrow went.
    struct Delivery {
        uint32 sourceDomain;
        uint64 nonce;
        bytes32 memberId;
        address sender;
        address recipient;
        address token;
        uint256 amount;
        bytes hookData;
    }

    enum MoveStatus {
        None,
        Pending,
        Completed,
        Cancelled
    }

    /// @notice A move out of this chain, by its TRANSFER nonce.
    struct Move {
        MoveStatus status;
        uint32 destDomain;
        uint64 deadline;
        bytes32 memberId;
        address returnTo;
        uint256 amount;
    }

    enum Inbound {
        None,
        Minted,
        Cancelled
    }

    /// @notice A token bucket: `capacity` refilling linearly over `window`.
    struct RateLimit {
        uint128 capacity;
        uint128 level;
        uint64 window;
        uint64 updatedAt;
    }

    uint32 public immutable LOCAL_DOMAIN;
    uint32 public immutable HOME_DOMAIN;
    OmnibusLedger public immutable LEDGER;

    uint8 public threshold;
    uint256 public attesterCount;
    mapping(address signer => bool) public isAttester;
    uint64 public nextNonce;
    mapping(uint32 domain => address messenger) public remoteMessenger;
    mapping(bytes32 memberId => address token) public localToken;
    mapping(uint32 domain => mapping(uint64 nonce => bool)) public usedNonce;

    /// @notice How long a move waits for its mint before it may be cancelled.
    uint64 public moveTimeout = 1 days;

    /// @notice Source side: moves out of this chain, and what they hold in
    ///         escrow here per member.
    mapping(uint64 nonce => Move) public moves;
    mapping(bytes32 memberId => uint256) public escrowed;

    /// @notice Destination side: what became of each inbound TRANSFER.
    mapping(uint32 domain => mapping(uint64 nonce => Inbound)) public inbound;

    /// @notice Home only: the most of a member's supply that may sit on a
    ///         chain; how much is minted there (acknowledged); and how much
    ///         is in escrow here on its way there.
    mapping(bytes32 memberId => mapping(uint32 domain => uint256)) public corridorCap;
    mapping(bytes32 memberId => mapping(uint32 domain => uint256)) public outstanding;
    mapping(bytes32 memberId => mapping(uint32 domain => uint256)) public pendingOut;

    /// @notice The issuing bank's own attestation key, per member.
    mapping(bytes32 memberId => address signer) public issuerAttester;
    mapping(address signer => bytes32 memberId) public issuerOfKey;

    /// @notice Not home: the most of a member's token that may exist on
    ///         this chain.
    mapping(bytes32 memberId => uint256) public supplyCap;

    /// @notice Where cancelled escrow goes when its `returnTo` cannot hold the token.
    mapping(bytes32 memberId => address wallet) public suspense;

    mapping(uint32 sourceDomain => mapping(bytes32 memberId => RateLimit)) internal _rateLimits;

    event MessageSent(bytes message);
    event MessageReceived(uint32 indexed sourceDomain, uint64 indexed nonce, bytes32 indexed memberId, address recipient, uint256 amount);
    event MoveLocked(uint64 indexed nonce, bytes32 indexed memberId, uint32 indexed destDomain, uint256 amount, uint64 deadline);
    event MoveCompleted(uint64 indexed nonce, bytes32 indexed memberId, uint32 indexed destDomain, uint256 amount);
    event MoveCancelled(uint64 indexed nonce, bytes32 indexed memberId, uint32 indexed destDomain, address to, uint256 amount);
    event MintCancelled(uint32 indexed sourceDomain, uint64 indexed nonce, bytes32 indexed memberId, uint256 amount);
    event ReturnedToSuspense(uint32 indexed sourceDomain, uint64 indexed nonce, bytes32 indexed memberId, address intended, uint256 amount);
    event AttesterSet(address indexed signer, bool enabled);
    event ThresholdSet(uint8 threshold);
    event RemoteSet(uint32 indexed domain, address indexed messenger);
    event TokenSet(bytes32 indexed memberId, address indexed token);
    event CorridorCapSet(bytes32 indexed memberId, uint32 indexed domain, uint256 cap);
    event IssuerAttesterSet(bytes32 indexed memberId, address signer);
    event SupplyCapSet(bytes32 indexed memberId, uint256 cap);
    event SuspenseSet(bytes32 indexed memberId, address wallet);
    event RateLimitSet(uint32 indexed sourceDomain, bytes32 indexed memberId, uint256 capacity, uint64 window);
    event MoveTimeoutSet(uint64 timeout);
    event RevocationApplied(uint32 indexed sourceDomain, uint64 indexed nonce, bytes32 indexed memberId, address account, bytes32 reason);

    error UnknownDomain(uint32 domain);
    error UnknownToken(bytes32 memberId);
    error ZeroAmount();
    error ZeroAddress();
    error BadVersion(uint8 version);
    error BadKind(uint8 kind);
    error WrongDestination(uint32 destination);
    error UntrustedSource(uint32 domain, address messenger);
    error NonceUsed(uint32 domain, uint64 nonce);
    error NotEnoughAttestations(uint256 valid, uint8 threshold);
    error SignersNotSorted();
    error NotAnAttester(address signer);
    error BadThreshold(uint8 threshold, uint256 attesters);
    error SpokeToSpoke(uint32 destination);
    error CorridorCapExceeded(bytes32 memberId, uint32 domain, uint256 wouldBe, uint256 cap);
    error CorridorUnderflow(bytes32 memberId, uint32 domain, uint256 amount, uint256 outstanding);
    error WrongCaller(address caller, address destinationCaller);
    error IssuerNotSet(bytes32 memberId);
    error BadIssuerAttestation(bytes32 memberId, address signer);
    error KeyHasOtherRole(address signer);
    error SupplyCapExceeded(bytes32 memberId, uint256 wouldBe, uint256 cap);
    error NotALowering(uint256 current, uint256 proposed);
    error RateLimited(uint32 sourceDomain, bytes32 memberId, uint256 amount, uint256 available);
    error BadRateLimit(uint256 capacity, uint64 window);
    error RateLimitNotLowered(uint256 capacity, uint64 window, uint256 proposedCapacity, uint64 proposedWindow);
    error BadMoveTimeout(uint64 timeout);
    error Expired(uint32 sourceDomain, uint64 nonce, uint64 deadline);
    error NotCancellable(uint8 kind);
    error NotYetCancellable(uint64 deadline, address recipient);
    error UnknownMove(uint32 domain, uint64 nonce);
    error NoSuspense(bytes32 memberId);
    error NotRegistrar(bytes32 memberId, address caller);
    error HomeOnly();

    /// @param admin in production, a TimelockController.
    constructor(uint32 localDomain, uint32 homeDomain, OmnibusLedger ledger, address admin, uint48 adminDelay)
        AccessControlDefaultAdminRules(adminDelay, admin)
    {
        if ((localDomain == homeDomain) != (address(ledger) != address(0))) revert UnknownDomain(localDomain);
        LOCAL_DOMAIN = localDomain;
        HOME_DOMAIN = homeDomain;
        LEDGER = ledger;
    }

    function _isHome() internal view returns (bool) {
        return LOCAL_DOMAIN == HOME_DOMAIN;
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Configuration
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Enabling or disabling an attester must keep the threshold a
    ///         strict majority of, and no more than, the enabled attesters:
    ///         raise the threshold before adding, lower it before removing.
    function setAttester(address signer, bool enabled) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (signer == address(0)) revert ZeroAddress();
        if (enabled == isAttester[signer]) return;
        if (enabled && issuerOfKey[signer] != bytes32(0)) revert KeyHasOtherRole(signer);
        uint256 count = enabled ? attesterCount + 1 : attesterCount - 1;
        if (threshold != 0) _checkThreshold(threshold, count);
        isAttester[signer] = enabled;
        attesterCount = count;
        emit AttesterSet(signer, enabled);
    }

    /// @notice The guardian removes a suspect attester at once (restrictive);
    ///         adding one is configuration, behind the timelock. The threshold
    ///         rules still hold, so lower the threshold first if needed.
    function disableAttester(address signer) external onlyRole(PAUSER_ROLE) {
        if (!isAttester[signer]) revert NotAnAttester(signer);
        uint256 count = attesterCount - 1;
        _checkThreshold(threshold, count);
        isAttester[signer] = false;
        attesterCount = count;
        emit AttesterSet(signer, false);
    }

    function setThreshold(uint8 t) external onlyRole(DEFAULT_ADMIN_ROLE) {
        _checkThreshold(t, attesterCount);
        threshold = t;
        emit ThresholdSet(t);
    }

    function _checkThreshold(uint8 t, uint256 count) internal pure {
        if (t == 0 || t > count || uint256(t) * 2 <= count) revert BadThreshold(t, count);
    }

    function setRemote(uint32 domain, address messenger) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (!_isHome() && domain != HOME_DOMAIN) revert SpokeToSpoke(domain);
        remoteMessenger[domain] = messenger;
        emit RemoteSet(domain, messenger);
    }

    function setToken(bytes32 memberId, address token) external onlyRole(DEFAULT_ADMIN_ROLE) {
        localToken[memberId] = token;
        emit TokenSet(memberId, token);
    }

    /// @notice Sets (or rotates) a member's issuer key on this chain.
    function setIssuerAttester(bytes32 memberId, address signer) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (signer == address(0)) revert ZeroAddress();
        if (isAttester[signer]) revert KeyHasOtherRole(signer);
        bytes32 holder = issuerOfKey[signer];
        if (holder != bytes32(0) && holder != memberId) revert KeyHasOtherRole(signer);
        address old = issuerAttester[memberId];
        if (old != address(0)) delete issuerOfKey[old];
        issuerAttester[memberId] = signer;
        issuerOfKey[signer] = memberId;
        emit IssuerAttesterSet(memberId, signer);
    }

    /// @notice The issuing bank's suspense wallet on this chain.
    function setSuspense(bytes32 memberId, address wallet) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (wallet == address(0)) revert ZeroAddress();
        suspense[memberId] = wallet;
        emit SuspenseSet(memberId, wallet);
    }

    /// @notice How long a new move waits for its mint before it may be
    ///         cancelled. Applies to moves sent after the change.
    function setMoveTimeout(uint64 timeout) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (timeout < MIN_MOVE_TIMEOUT || timeout > MAX_MOVE_TIMEOUT) revert BadMoveTimeout(timeout);
        moveTimeout = timeout;
        emit MoveTimeoutSet(timeout);
    }

    /// @notice Not home. Raising goes through the admin (the timelock).
    function setSupplyCap(bytes32 memberId, uint256 cap) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (_isHome()) revert UnknownDomain(LOCAL_DOMAIN);
        supplyCap[memberId] = cap;
        emit SupplyCapSet(memberId, cap);
    }

    /// @notice Home only. Lowering a cap below what is outstanding stops new
    ///         transfers out; tokens already there can still come home.
    function setCorridorCap(bytes32 memberId, uint32 domain, uint256 cap) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (!_isHome() || domain == HOME_DOMAIN) revert UnknownDomain(domain);
        corridorCap[memberId][domain] = cap;
        emit CorridorCapSet(memberId, domain, cap);
    }

    /// @notice The guardian lowers a corridor cap at once (restrictive).
    function lowerCorridorCap(bytes32 memberId, uint32 domain, uint256 cap) external onlyRole(PAUSER_ROLE) {
        if (!_isHome() || domain == HOME_DOMAIN) revert UnknownDomain(domain);
        uint256 current = corridorCap[memberId][domain];
        if (cap >= current) revert NotALowering(current, cap);
        corridorCap[memberId][domain] = cap;
        emit CorridorCapSet(memberId, domain, cap);
    }

    /// @notice The guardian lowers a supply cap at once (restrictive).
    function lowerSupplyCap(bytes32 memberId, uint256 cap) external onlyRole(PAUSER_ROLE) {
        if (_isHome()) revert UnknownDomain(LOCAL_DOMAIN);
        uint256 current = supplyCap[memberId];
        if (cap >= current) revert NotALowering(current, cap);
        supplyCap[memberId] = cap;
        emit SupplyCapSet(memberId, cap);
    }

    /// @notice Opens, resizes or closes (capacity zero) the inbound bucket for
    ///         a member's messages from `sourceDomain`. A newly opened bucket
    ///         starts full; otherwise its current level is kept, up to the new
    ///         capacity.
    function setRateLimit(uint32 sourceDomain, bytes32 memberId, uint128 capacity, uint64 window)
        external
        onlyRole(DEFAULT_ADMIN_ROLE)
    {
        if (sourceDomain == LOCAL_DOMAIN) revert UnknownDomain(sourceDomain);
        if (capacity != 0 && window == 0) revert BadRateLimit(capacity, window);
        _setRateLimit(sourceDomain, memberId, capacity, window);
    }

    /// @notice The guardian tightens a bucket at once: a smaller capacity, a
    ///         longer window (a slower refill), or both; never the reverse.
    function lowerRateLimit(uint32 sourceDomain, bytes32 memberId, uint128 capacity, uint64 window)
        external
        onlyRole(PAUSER_ROLE)
    {
        RateLimit memory r = _rateLimits[sourceDomain][memberId];
        bool tighter = capacity <= r.capacity && window >= r.window && (capacity < r.capacity || window > r.window);
        if (!tighter) revert RateLimitNotLowered(r.capacity, r.window, capacity, window);
        _setRateLimit(sourceDomain, memberId, capacity, window);
    }

    function _setRateLimit(uint32 sourceDomain, bytes32 memberId, uint128 capacity, uint64 window) internal {
        RateLimit storage r = _rateLimits[sourceDomain][memberId];
        uint256 available = _available(r);
        bool opening = r.capacity == 0;
        r.capacity = capacity;
        r.window = window;
        r.level = opening ? capacity : uint128(available < capacity ? available : capacity);
        r.updatedAt = uint64(block.timestamp);
        emit RateLimitSet(sourceDomain, memberId, capacity, window);
    }

    /// @notice A bucket's settings and what it would let through now.
    function rateLimit(uint32 sourceDomain, bytes32 memberId)
        external
        view
        returns (uint256 capacity, uint64 window, uint256 available)
    {
        RateLimit memory r = _rateLimits[sourceDomain][memberId];
        return (r.capacity, r.window, _available(r));
    }

    /// @dev A closed bucket (capacity 0) yields 0 on both branches; a zero
    ///      window only exists with capacity 0 and takes the first branch.
    function _available(RateLimit memory r) internal view returns (uint256) {
        uint256 elapsed = block.timestamp - r.updatedAt;
        if (elapsed >= r.window) return r.capacity;
        uint256 a = uint256(r.level) + elapsed * r.capacity / r.window;
        return a < r.capacity ? a : r.capacity;
    }

    /// @dev Draws `amount` from the bucket, or reverts so the message can be
    ///      retried once it has refilled.
    function _consume(uint32 sourceDomain, bytes32 memberId, uint256 amount) internal {
        RateLimit storage r = _rateLimits[sourceDomain][memberId];
        uint256 available = _available(r);
        if (amount > available) revert RateLimited(sourceDomain, memberId, amount, available);
        // available <= capacity, a uint128.
        // forge-lint: disable-next-line(unsafe-typecast)
        r.level = uint128(available - amount);
        r.updatedAt = uint64(block.timestamp);
    }

    function pause() external onlyRole(PAUSER_ROLE) {
        _pause();
    }

    function unpause() external onlyRole(PAUSER_ROLE) {
        _unpause();
    }

    /*//////////////////////////////////////////////////////////////////////////
                                Lock here (send)
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Locks the caller's tokens in escrow and emits the message that
    ///         lets the destination mint them to `recipient`. The escrow burns
    ///         when the mint is acknowledged, or returns to the caller if the
    ///         move is cancelled.
    function depositForBurn(bytes32 memberId, uint256 amount, uint32 destDomain, address recipient)
        external
        returns (uint64 nonce)
    {
        return _depositForBurn(memberId, amount, destDomain, recipient, address(0), "", msg.sender);
    }

    /// @notice As depositForBurn, but only `destinationCaller` may deliver
    ///         the message, it receives `hookData` with the tokens, and a
    ///         cancellation returns them to `returnTo` (the caller when zero).
    function depositForBurnWithHook(
        bytes32 memberId,
        uint256 amount,
        uint32 destDomain,
        address recipient,
        address destinationCaller,
        bytes calldata hookData,
        address returnTo
    ) external returns (uint64 nonce) {
        return _depositForBurn(
            memberId, amount, destDomain, recipient, destinationCaller, hookData, returnTo == address(0) ? msg.sender : returnTo
        );
    }

    function _depositForBurn(
        bytes32 memberId,
        uint256 amount,
        uint32 destDomain,
        address recipient,
        address destinationCaller,
        bytes memory hookData,
        address returnTo
    ) internal whenNotPaused returns (uint64 nonce) {
        if (amount == 0) revert ZeroAmount();
        if (recipient == address(0)) revert ZeroAddress();
        if (remoteMessenger[destDomain] == address(0)) revert UnknownDomain(destDomain);
        if (!_isHome() && destDomain != HOME_DOMAIN) revert SpokeToSpoke(destDomain);
        address token = localToken[memberId];
        if (token == address(0)) revert UnknownToken(memberId);

        if (_isHome()) {
            // What is minted there, what is on its way, and this move.
            uint256 wouldBe = outstanding[memberId][destDomain] + pendingOut[memberId][destDomain] + amount;
            uint256 cap = corridorCap[memberId][destDomain];
            if (wouldBe > cap) revert CorridorCapExceeded(memberId, destDomain, wouldBe, cap);
            pendingOut[memberId][destDomain] += amount;
        }

        uint64 deadline = uint64(block.timestamp) + moveTimeout;
        nonce = nextNonce;
        moves[nonce] = Move({
            status: MoveStatus.Pending,
            destDomain: destDomain,
            deadline: deadline,
            memberId: memberId,
            returnTo: returnTo,
            amount: amount
        });
        escrowed[memberId] += amount;
        _emit(
            Envelope({
                version: VERSION,
                kind: KIND_TRANSFER,
                sourceDomain: LOCAL_DOMAIN,
                destDomain: destDomain,
                nonce: 0, // set by _emit
                sourceMessenger: address(this),
                memberId: memberId,
                sender: msg.sender,
                recipient: recipient,
                amount: amount,
                destinationCaller: destinationCaller,
                returnTo: returnTo,
                deadline: deadline,
                refNonce: 0,
                hookData: hookData
            })
        );
        emit MoveLocked(nonce, memberId, destDomain, amount, deadline);

        IBurnMintToken(token).messengerLock(msg.sender, amount);
    }

    /// @dev Stamps the next nonce and this chain's identity on `e` and emits it.
    function _emit(Envelope memory e) internal returns (uint64 nonce) {
        nonce = nextNonce++;
        e.nonce = nonce;
        e.sourceDomain = LOCAL_DOMAIN;
        e.sourceMessenger = address(this);
        e.version = VERSION;
        emit MessageSent(abi.encode(e));
    }

    /// @dev The reply to an inbound TRANSFER `t`, back to its source.
    function _reply(uint8 kind, Envelope memory t) internal returns (uint64) {
        return _emit(
            Envelope({
                version: VERSION,
                kind: kind,
                sourceDomain: LOCAL_DOMAIN,
                destDomain: t.sourceDomain,
                nonce: 0,
                sourceMessenger: address(this),
                memberId: t.memberId,
                sender: t.sender,
                recipient: t.recipient,
                amount: t.amount,
                destinationCaller: address(0),
                returnTo: t.returnTo,
                deadline: 0,
                refNonce: t.nonce,
                hookData: ""
            })
        );
    }

    /// @notice Home only: the member's registrar broadcasts that `account`
    ///         may no longer hold the member's token on `destDomain`.
    function sendRevocation(bytes32 memberId, address account, uint32 destDomain, bytes32 reason)
        external
        whenNotPaused
        returns (uint64 nonce)
    {
        if (!_isHome()) revert HomeOnly();
        if (remoteMessenger[destDomain] == address(0)) revert UnknownDomain(destDomain);
        if (account == address(0)) revert ZeroAddress();
        address token = localToken[memberId];
        if (token == address(0)) revert UnknownToken(memberId);
        address registry = address(IPolicyHolder(token).policy());
        if (!IAccessControlView(registry).hasRole(REGISTRAR_ROLE, msg.sender)) revert NotRegistrar(memberId, msg.sender);
        nonce = _emit(
            Envelope({
                version: VERSION,
                kind: KIND_POLICY,
                sourceDomain: LOCAL_DOMAIN,
                destDomain: destDomain,
                nonce: 0,
                sourceMessenger: address(this),
                memberId: memberId,
                sender: msg.sender,
                recipient: account,
                amount: 0,
                destinationCaller: address(0),
                returnTo: address(0),
                deadline: 0,
                refNonce: 0,
                hookData: abi.encode(reason)
            })
        );
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Receive
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Applies an attested message from a trusted source domain: a
    ///         TRANSFER mints here; a MINT_ACK burns escrow here; a
    ///         MINT_CANCEL returns escrow here; a POLICY revokes a holder.
    /// @param signatures the operator's threshold signatures over the message, ordered
    ///        by ascending signer address.
    /// @param issuerSignature the issuing bank's signature over the same
    ///        message, by its key on this chain.
    function receiveMessage(bytes calldata message, bytes[] calldata signatures, bytes calldata issuerSignature)
        external
        whenNotPaused
        returns (Delivery memory d)
    {
        Envelope memory e = _open(message, signatures, issuerSignature);
        if (e.destinationCaller != address(0) && msg.sender != e.destinationCaller) {
            revert WrongCaller(msg.sender, e.destinationCaller);
        }
        usedNonce[e.sourceDomain][e.nonce] = true;

        address token = localToken[e.memberId];
        if (token == address(0)) revert UnknownToken(e.memberId);
        d.sourceDomain = e.sourceDomain;
        d.nonce = e.nonce;
        d.memberId = e.memberId;
        d.sender = e.sender;
        d.token = token;

        if (e.kind == KIND_TRANSFER) {
            d.recipient = e.recipient;
            d.amount = e.amount;
            d.hookData = e.hookData;
            _mintTransfer(e, token);
        } else if (e.kind == KIND_MINT_ACK) {
            _completeMove(e, token);
        } else if (e.kind == KIND_MINT_CANCEL) {
            d.amount = e.amount;
            d.recipient = _cancelMove(e, token);
        } else if (e.kind == KIND_POLICY) {
            bytes32 reason = abi.decode(e.hookData, (bytes32));
            d.recipient = e.recipient;
            emit RevocationApplied(e.sourceDomain, e.nonce, e.memberId, e.recipient, reason);
            IRevocable(address(IPolicyHolder(token).policy())).revoke(e.recipient, reason);
        } else {
            revert BadKind(e.kind);
        }
    }

    /// @dev Destination of a TRANSFER: caps, rate limit, mint, MINT_ACK back.
    function _mintTransfer(Envelope memory e, address token) internal {
        if (block.timestamp >= e.deadline) revert Expired(e.sourceDomain, e.nonce, e.deadline);
        if (_isHome()) {
            uint256 out = outstanding[e.memberId][e.sourceDomain];
            if (e.amount > out) revert CorridorUnderflow(e.memberId, e.sourceDomain, e.amount, out);
            outstanding[e.memberId][e.sourceDomain] = out - e.amount;
        } else {
            uint256 wouldBe = IERC20(token).totalSupply() + e.amount;
            uint256 cap = supplyCap[e.memberId];
            if (wouldBe > cap) revert SupplyCapExceeded(e.memberId, wouldBe, cap);
        }
        _consume(e.sourceDomain, e.memberId, e.amount);
        inbound[e.sourceDomain][e.nonce] = Inbound.Minted;
        emit MessageReceived(e.sourceDomain, e.nonce, e.memberId, e.recipient, e.amount);
        _reply(KIND_MINT_ACK, e);

        if (_isHome()) LEDGER.recordRemote(e.memberId, -int256(e.amount));
        IBurnMintToken(token).messengerMint(e.recipient, e.amount);
    }

    /// @dev Source of a move, on its MINT_ACK: the escrow burns, and home
    ///      counts the amount abroad from now on.
    function _completeMove(Envelope memory e, address token) internal {
        Move storage mv = _pendingMove(e);
        mv.status = MoveStatus.Completed;
        escrowed[e.memberId] -= e.amount;
        emit MoveCompleted(e.refNonce, e.memberId, e.sourceDomain, e.amount);
        if (_isHome()) {
            pendingOut[e.memberId][e.sourceDomain] -= e.amount;
            outstanding[e.memberId][e.sourceDomain] += e.amount;
            LEDGER.recordRemote(e.memberId, int256(e.amount));
        }
        IBurnMintToken(token).messengerBurnLocked(e.amount);
    }

    /// @dev Source of a move, on its MINT_CANCEL: the escrow returns to
    ///      `returnTo`, or to the bank's suspense wallet if `returnTo` cannot
    ///      hold the token. Released value draws on the rate limit like a mint.
    function _cancelMove(Envelope memory e, address token) internal returns (address to) {
        Move storage mv = _pendingMove(e);
        _consume(e.sourceDomain, e.memberId, e.amount);
        mv.status = MoveStatus.Cancelled;
        escrowed[e.memberId] -= e.amount;
        if (_isHome()) pendingOut[e.memberId][e.sourceDomain] -= e.amount;

        to = mv.returnTo;
        if (!_canReceive(token, to)) {
            to = suspense[e.memberId];
            if (to == address(0)) revert NoSuspense(e.memberId);
            emit ReturnedToSuspense(e.sourceDomain, e.refNonce, e.memberId, mv.returnTo, e.amount);
        }
        emit MoveCancelled(e.refNonce, e.memberId, e.sourceDomain, to, e.amount);
        IBurnMintToken(token).messengerUnlock(to, e.amount);
    }

    /// @dev The pending move a reply refers to; the reply must come from the
    ///      move's destination and repeat its member and amount.
    function _pendingMove(Envelope memory e) internal view returns (Move storage mv) {
        mv = moves[e.refNonce];
        if (
            mv.status != MoveStatus.Pending || mv.destDomain != e.sourceDomain || mv.memberId != e.memberId
                || mv.amount != e.amount
        ) revert UnknownMove(e.sourceDomain, e.refNonce);
    }

    /// @notice Records an inbound TRANSFER as never to be minted here and
    ///         sends a MINT_CANCEL back, so its source returns the escrow.
    ///         Anyone may call it with the message's attestations, once its
    ///         deadline has passed, or at once if its recipient cannot hold the
    ///         token here (or the token is unknown here). Works while paused.
    function cancel(bytes calldata message, bytes[] calldata signatures, bytes calldata issuerSignature)
        external
        returns (uint64 cancelNonce)
    {
        Envelope memory e = _open(message, signatures, issuerSignature);
        if (e.kind != KIND_TRANSFER) revert NotCancellable(e.kind);
        if (block.timestamp < e.deadline) {
            address token = localToken[e.memberId];
            if (token != address(0) && _canReceive(token, e.recipient)) revert NotYetCancellable(e.deadline, e.recipient);
        }
        usedNonce[e.sourceDomain][e.nonce] = true;
        inbound[e.sourceDomain][e.nonce] = Inbound.Cancelled;
        emit MintCancelled(e.sourceDomain, e.nonce, e.memberId, e.amount);
        cancelNonce = _reply(KIND_MINT_CANCEL, e);
    }

    /// @dev Checks everything common to receiving and cancelling: attestations,
    ///      version, destination, source messenger, unused nonce, issuer key.
    function _open(bytes calldata message, bytes[] calldata signatures, bytes calldata issuerSignature)
        internal
        view
        returns (Envelope memory e)
    {
        _verify(message, signatures);
        e = abi.decode(message, (Envelope));
        if (e.version != VERSION) revert BadVersion(e.version);
        if (e.destDomain != LOCAL_DOMAIN) revert WrongDestination(e.destDomain);
        if (e.sourceMessenger == address(0) || e.sourceMessenger != remoteMessenger[e.sourceDomain]) {
            revert UntrustedSource(e.sourceDomain, e.sourceMessenger);
        }
        if (usedNonce[e.sourceDomain][e.nonce]) revert NonceUsed(e.sourceDomain, e.nonce);
        address issuer = issuerAttester[e.memberId];
        if (issuer == address(0)) revert IssuerNotSet(e.memberId);
        address signed = ECDSA.recover(MessageHashUtils.toEthSignedMessageHash(keccak256(message)), issuerSignature);
        if (signed != issuer) revert BadIssuerAttestation(e.memberId, signed);
    }

    function _canReceive(address token, address account) internal view returns (bool) {
        try IERC7943Receive(token).canReceive(account) returns (bool ok) {
            return ok;
        } catch {
            return true; // a token without the check accepts anyone
        }
    }

    function _verify(bytes calldata message, bytes[] calldata signatures) internal view {
        bytes32 digest = MessageHashUtils.toEthSignedMessageHash(keccak256(message));
        address last;
        uint256 valid;
        for (uint256 i = 0; i < signatures.length; i++) {
            address signer = ECDSA.recover(digest, signatures[i]);
            if (signer <= last) revert SignersNotSorted();
            if (!isAttester[signer]) revert NotAnAttester(signer);
            last = signer;
            valid++;
        }
        if (threshold == 0 || valid < threshold) revert NotEnoughAttestations(valid, threshold);
    }

}

/// @dev What the messenger asks of a token and its holder registry.
interface IPolicyHolder {

    function policy() external view returns (address);

}

interface IAccessControlView {

    function hasRole(bytes32 role, address account) external view returns (bool);

}

/// @dev The one ERC-7943 view the messenger asks a token.
interface IERC7943Receive {

    function canReceive(address account) external view returns (bool);

}
