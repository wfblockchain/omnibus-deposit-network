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
 * @notice Moves a member's tokenized deposits between chains the way Circle
 *         moves USDC with CCTP: burn on the source chain, an attested
 *         message, mint on the destination chain. Nothing is wrapped and no
 *         pool of locked tokens exists anywhere.
 *
 * @dev THE BACKING NEVER MOVES. Reserves stay in the operator's joint account at the
 *      Fed, recorded by the OmnibusLedger on the home chain. When tokens
 *      leave home, the home messenger burns them and records the amount as
 *      the member's remote supply; the ledger's invariant becomes
 *      backing == home supply + remote supply. When tokens come home, the
 *      remote supply falls by what is minted. While a message is in flight
 *      in either direction the recorded remote supply is at least what
 *      exists remotely, so tokens are never under-backed.
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
 *      DELIVERY WITH INSTRUCTIONS. As in CCTP V2, a message may name a
 *      `destinationCaller`, the only address allowed to deliver it, and
 *      carry `hookData` for that caller. A settlement contract uses the pair
 *      to receive cash and act on it in the same transaction: it delivers
 *      the message itself, the tokens mint to it, and it reads the
 *      instruction from what `receiveMessage` returns. Nobody else can
 *      deliver the message first and leave the tokens unaccounted for.
 *
 *      TWO KEYS FOR EVERY MINT. A token is its issuing bank's liability, so
 *      no one but that bank should be able to create it anywhere. A message
 *      mints only with the operator's attester threshold AND a signature from the
 *      issuing bank's own attestation key, set per member on each chain. A
 *      compromised operator attester set cannot mint a bank's deposits, and a
 *      compromised bank key cannot mint without the operator. A key can never hold
 *      both roles.
 *
 *      CAPS ON BOTH SIDES, as Circle caps each minter with an allowance.
 *      Home caps each corridor; every other chain caps each member's supply
 *      there. Even if both key sets were compromised, a forged mint on another
 *      chain stops at that chain's cap, and home refuses to take back more
 *      than it sent.
 *
 *      GOVERNANCE. Configuration (attesters, threshold, issuer keys, remotes,
 *      tokens, caps) belongs to the default admin, which in production is a
 *      TimelockController: every change is visible for the timelock's delay
 *      before it takes effect. Handing the admin role over is itself two-step
 *      and delayed. Pausing is a separate role and takes effect at once.
 *
 *      FINALITY is the attesters' duty, not something a destination can
 *      check: they sign a message only once its burn is final at the source.
 *
 *      PAUSE. Stops sending and delivery on this chain. Tokens already
 *      burned stay counted as remote supply at home, so a pause never leaves
 *      tokens under-backed; delivery resumes where it stopped. The pauser is
 *      also the guardian: it may LOWER a corridor or supply cap at once.
 *      Raising one is configuration, behind the timelock.
 *
 *      UNDELIVERABLE MESSAGES COME BACK. A transfer whose recipient cannot hold
 *      the token on the destination (not admitted, blocked) can never mint
 *      there, and its tokens are already burned at the source. Anyone may
 *      `bounce` it with the same attestations: the destination consumes the
 *      nonce, so it can never mint later, and sends a RETURN message that
 *      re-mints the amount to the sender's `returnTo` on the source. A RETURN
 *      is never bounced: if `returnTo` cannot hold the token either, the
 *      amount mints to the issuing bank's suspense wallet on that chain, the
 *      way a bank books an unapplied payment to a suspense account.
 *      Accounting follows the tokens: home's corridor count still includes a
 *      bounced amount until the RETURN lands.
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

    uint8 public constant VERSION = 3;

    uint8 public constant KIND_TRANSFER = 0;
    uint8 public constant KIND_RETURN = 1;
    uint8 public constant KIND_POLICY = 2;

    bytes32 private constant REGISTRAR_ROLE = keccak256("REGISTRAR_ROLE");

    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");

    /// @notice The attested message. Encoded with `abi.encode(envelope)`.
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
        bytes hookData;
    }

    /// @notice What `receiveMessage` delivered, for a destination caller
    ///         that acts on it.
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

    /// @notice Home only: the most of a member's supply that may sit on a
    ///         chain, and how much sits there now (including in flight).
    mapping(bytes32 memberId => mapping(uint32 domain => uint256)) public corridorCap;
    mapping(bytes32 memberId => mapping(uint32 domain => uint256)) public outstanding;

    /// @notice The issuing bank's own attestation key, per member.
    mapping(bytes32 memberId => address signer) public issuerAttester;
    mapping(address signer => bytes32 memberId) public issuerOfKey;

    /// @notice Not home: the most of a member's token that may exist on
    ///         this chain.
    mapping(bytes32 memberId => uint256) public supplyCap;

    /// @notice Where a RETURN mints when its `returnTo` cannot hold the token.
    mapping(bytes32 memberId => address wallet) public suspense;

    event MessageSent(bytes message);
    event MessageReceived(uint32 indexed sourceDomain, uint64 indexed nonce, bytes32 indexed memberId, address recipient, uint256 amount);
    event Bounced(uint32 indexed sourceDomain, uint64 indexed nonce, bytes32 indexed memberId, address returnTo, uint256 amount);
    event ReturnedToSuspense(uint32 indexed sourceDomain, uint64 indexed nonce, bytes32 indexed memberId, address intended, uint256 amount);
    event AttesterSet(address indexed signer, bool enabled);
    event ThresholdSet(uint8 threshold);
    event RemoteSet(uint32 indexed domain, address indexed messenger);
    event TokenSet(bytes32 indexed memberId, address indexed token);
    event CorridorCapSet(bytes32 indexed memberId, uint32 indexed domain, uint256 cap);
    event IssuerAttesterSet(bytes32 indexed memberId, address signer);
    event SupplyCapSet(bytes32 indexed memberId, uint256 cap);
    event SuspenseSet(bytes32 indexed memberId, address wallet);
    event RevocationApplied(uint32 indexed sourceDomain, uint64 indexed nonce, bytes32 indexed memberId, address account, bytes32 reason);

    error UnknownDomain(uint32 domain);
    error UnknownToken(bytes32 memberId);
    error ZeroAmount();
    error ZeroAddress();
    error BadVersion(uint8 version);
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
    error NotBounceable(uint8 kind);
    error StillDeliverable(address recipient);
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

    function pause() external onlyRole(PAUSER_ROLE) {
        _pause();
    }

    function unpause() external onlyRole(PAUSER_ROLE) {
        _unpause();
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Burn here
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Burns the caller's tokens and emits the message that lets the
    ///         destination mint them to `recipient`. A bounce returns them to
    ///         the caller.
    function depositForBurn(bytes32 memberId, uint256 amount, uint32 destDomain, address recipient)
        external
        returns (uint64 nonce)
    {
        return _depositForBurn(memberId, amount, destDomain, recipient, address(0), "", msg.sender);
    }

    /// @notice As depositForBurn, but only `destinationCaller` may deliver
    ///         the message, it receives `hookData` with the tokens, and a
    ///         bounce returns them to `returnTo` (the caller when zero).
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
            uint256 wouldBe = outstanding[memberId][destDomain] + amount;
            uint256 cap = corridorCap[memberId][destDomain];
            if (wouldBe > cap) revert CorridorCapExceeded(memberId, destDomain, wouldBe, cap);
            outstanding[memberId][destDomain] = wouldBe;
        }

        IBurnMintToken(token).messengerBurn(msg.sender, amount);
        if (_isHome()) LEDGER.recordRemote(memberId, int256(amount));

        nonce = _emit(KIND_TRANSFER, destDomain, memberId, msg.sender, recipient, amount, destinationCaller, returnTo, hookData);
    }

    function _emit(
        uint8 kind,
        uint32 destDomain,
        bytes32 memberId,
        address sender,
        address recipient,
        uint256 amount,
        address destinationCaller,
        address returnTo,
        bytes memory hookData
    ) internal returns (uint64 nonce) {
        nonce = nextNonce++;
        emit MessageSent(
            abi.encode(
                Envelope({
                    version: VERSION,
                    kind: kind,
                    sourceDomain: LOCAL_DOMAIN,
                    destDomain: destDomain,
                    nonce: nonce,
                    sourceMessenger: address(this),
                    memberId: memberId,
                    sender: sender,
                    recipient: recipient,
                    amount: amount,
                    destinationCaller: destinationCaller,
                    returnTo: returnTo,
                    hookData: hookData
                })
            )
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
        nonce = _emit(KIND_POLICY, destDomain, memberId, msg.sender, account, 0, address(0), address(0), abi.encode(reason));
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Mint there
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Mints on an attested message from a trusted source domain.
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
        if (e.kind == KIND_POLICY) {
            bytes32 reason = abi.decode(e.hookData, (bytes32));
            IRevocable(address(IPolicyHolder(token).policy())).revoke(e.recipient, reason);
            emit RevocationApplied(e.sourceDomain, e.nonce, e.memberId, e.recipient, reason);
            d.sourceDomain = e.sourceDomain;
            d.nonce = e.nonce;
            d.memberId = e.memberId;
            d.sender = e.sender;
            d.recipient = e.recipient;
            d.token = token;
            return d;
        }
        // A RETURN restores supply that left this chain, so it is not capped.
        if (!_isHome() && e.kind == KIND_TRANSFER) {
            uint256 wouldBe = IERC20(token).totalSupply() + e.amount;
            uint256 cap = supplyCap[e.memberId];
            if (wouldBe > cap) revert SupplyCapExceeded(e.memberId, wouldBe, cap);
        }
        if (_isHome()) {
            uint256 out = outstanding[e.memberId][e.sourceDomain];
            if (e.amount > out) revert CorridorUnderflow(e.memberId, e.sourceDomain, e.amount, out);
            outstanding[e.memberId][e.sourceDomain] = out - e.amount;
            LEDGER.recordRemote(e.memberId, -int256(e.amount));
        }

        address to = e.recipient;
        if (e.kind == KIND_RETURN && !_canReceive(token, to)) {
            to = suspense[e.memberId];
            if (to == address(0)) revert NoSuspense(e.memberId);
            emit ReturnedToSuspense(e.sourceDomain, e.nonce, e.memberId, e.recipient, e.amount);
        }
        IBurnMintToken(token).messengerMint(to, e.amount);
        emit MessageReceived(e.sourceDomain, e.nonce, e.memberId, to, e.amount);

        d = Delivery({
            sourceDomain: e.sourceDomain,
            nonce: e.nonce,
            memberId: e.memberId,
            sender: e.sender,
            recipient: to,
            token: token,
            amount: e.amount,
            hookData: e.hookData
        });
    }

    /// @notice Sends an undeliverable transfer back. Anyone may call it with
    ///         the message's attestations, once its recipient cannot hold the
    ///         token here (or the token is unknown here). The nonce is spent,
    ///         so the transfer can never mint here afterwards.
    function bounce(bytes calldata message, bytes[] calldata signatures, bytes calldata issuerSignature)
        external
        whenNotPaused
        returns (uint64 returnNonce)
    {
        Envelope memory e = _open(message, signatures, issuerSignature);
        if (e.kind != KIND_TRANSFER) revert NotBounceable(e.kind);
        address token = localToken[e.memberId];
        if (token != address(0) && _canReceive(token, e.recipient)) revert StillDeliverable(e.recipient);
        usedNonce[e.sourceDomain][e.nonce] = true;

        // Nothing was minted here and nothing moves in the ledger: at home a
        // bounced inbound amount is still counted abroad, where the RETURN
        // re-mints it; abroad, the RETURN takes it off home's count on arrival.
        emit Bounced(e.sourceDomain, e.nonce, e.memberId, e.returnTo, e.amount);
        returnNonce = _emit(
            KIND_RETURN,
            e.sourceDomain,
            e.memberId,
            e.recipient,
            e.returnTo,
            e.amount,
            address(0),
            address(0),
            abi.encode(e.sourceDomain, e.nonce)
        );
    }

    /// @dev Checks everything common to delivering and bouncing: attestations,
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
