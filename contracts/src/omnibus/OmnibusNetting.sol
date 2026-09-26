// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { AccessControlDefaultAdminRules } from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { OmnibusLedger } from "./OmnibusLedger.sol";

/**
 * @title OmnibusNetting
 * @notice Interbank obligations between members, settled either gross (one at
 *         a time, now) or in multilateral netting cycles, against members'
 *         free positions in the omnibus.
 *
 * @dev WHERE NETTING SAVES LIQUIDITY. A customer paying in tokens needs no
 *      bank liquidity: the reserves travel with the tokens. Liquidity is
 *      spent when a bank pays out of its own free position: a customer paid
 *      from a deposit account, a bank's own obligation, the interbank leg of
 *      a batch. Settled gross, every such payment needs the payer's free
 *      position to cover it. Queued and netted, offsetting obligations cancel
 *      and each bank funds only its net debit, which is how CHIPS settles
 *      about $2 trillion a day on roughly $96 billion of prefunding.
 *
 *      GROSS OR NET, PER OBLIGATION. Submitting moves nothing. The payer can
 *      take any queued obligation out and settle it gross at once
 *      (settleGross), paying for certainty with liquidity; the rest wait for
 *      the next cycle.
 *
 *      THE CHAIN DOES NOT TRUST THE PLANNER. The operator submits a plan:
 *      the obligations to discharge and each member's net position. The
 *      contract recomputes every net from the obligations themselves and
 *      rejects any mismatch; the ledger then refuses any net debit a member
 *      cannot fund from free position, so a plan can never create credit.
 *      Only the operator submits plans (a permissionless book could be
 *      parked with an unfundable plan), and ids must be strictly increasing,
 *      which makes the duplicate check linear.
 *
 *      OBLIGATIONS EXPIRE. Each carries a time-to-live; an unsettled one can
 *      be expired by anyone and never settles later.
 */
contract OmnibusNetting is AccessControlDefaultAdminRules, Pausable {

    bytes32 public constant OPERATOR_ROLE = keccak256("OPERATOR_ROLE");
    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");

    uint32 public constant MIN_TTL = 1 hours;
    uint32 public constant MAX_TTL = 7 days;

    OmnibusLedger public immutable LEDGER;

    enum Status {
        None,
        Queued,
        Settled,
        Cancelled,
        Expired
    }

    struct Obligation {
        bytes32 payer;
        bytes32 payee;
        uint128 amount;
        uint64 expiresAt;
        Status status;
    }

    mapping(bytes32 obligationId => Obligation) public obligations;
    mapping(bytes32 cycleId => bool) public cycleSettled;
    uint32 public ttl = 1 days;

    /// @notice Value discharged in cycles, and value that actually moved.
    uint256 public grossDischarged;
    uint256 public netMoved;
    /// @notice Value settled gross, outside cycles.
    uint256 public grossSettled;

    event ObligationSubmitted(
        bytes32 indexed obligationId, bytes32 indexed payer, bytes32 indexed payee, uint128 amount, uint64 expiresAt
    );
    event ObligationSettled(bytes32 indexed obligationId, bytes32 indexed cycleId);
    event ObligationCancelled(bytes32 indexed obligationId);
    event ObligationExpired(bytes32 indexed obligationId);
    event CycleSettled(bytes32 indexed cycleId, uint256 obligations, uint256 grossValue, uint256 netValue);
    event TtlChanged(uint32 ttl);

    error NotTheOperator(bytes32 memberId, address caller);
    error UnknownMember(bytes32 memberId);
    error PayeeSuspended(bytes32 memberId);
    error SamePayerAndPayee(bytes32 memberId);
    error ZeroAmount();
    error DuplicateObligation(bytes32 obligationId);
    error NotQueued(bytes32 obligationId, Status status);
    error ObligationHasExpired(bytes32 obligationId, uint64 expiresAt);
    error NotYetExpired(bytes32 obligationId, uint64 expiresAt);
    error CycleAlreadySettled(bytes32 cycleId);
    error LengthMismatch();
    error NotStrictlyIncreasing(uint256 index);
    error UnknownMemberInNetSet(bytes32 memberId);
    error NetMismatch(bytes32 memberId, int256 submitted, int256 computed);
    error BadTtl(uint32 ttl);

    constructor(OmnibusLedger ledger, address admin, uint48 adminDelay) AccessControlDefaultAdminRules(adminDelay, admin) {
        LEDGER = ledger;
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Obligations
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice The payer's operator records an obligation to another member.
    ///         Nothing moves. The id derives from the payer and its own
    ///         reference, so nobody else can occupy it.
    function submit(bytes32 payer, bytes32 payee, uint128 amount, bytes32 clientRef)
        external
        whenNotPaused
        returns (bytes32 id)
    {
        _onlyOperatorOf(payer);
        if (amount == 0) revert ZeroAmount();
        if (payer == payee) revert SamePayerAndPayee(payer);
        OmnibusLedger.Member memory t = LEDGER.member(payee);
        if (!t.admitted) revert UnknownMember(payee);
        if (t.suspended) revert PayeeSuspended(payee);

        id = keccak256(abi.encode(block.chainid, address(this), payer, clientRef));
        if (obligations[id].status != Status.None) revert DuplicateObligation(id);
        uint64 exp = uint64(block.timestamp) + ttl;
        obligations[id] = Obligation({ payer: payer, payee: payee, amount: amount, expiresAt: exp, status: Status.Queued });
        emit ObligationSubmitted(id, payer, payee, amount, exp);
    }

    /// @notice Gross settlement: the payer settles one queued obligation now,
    ///         from its free position.
    function settleGross(bytes32 id) external whenNotPaused {
        Obligation storage o = _live(id);
        _onlyOperatorOf(o.payer);
        o.status = Status.Settled;
        grossSettled += o.amount;
        LEDGER.transferFree(o.payer, o.payee, o.amount);
        emit ObligationSettled(id, bytes32(0));
    }

    function cancel(bytes32 id) external {
        Obligation storage o = obligations[id];
        if (o.status != Status.Queued) revert NotQueued(id, o.status);
        _onlyOperatorOf(o.payer);
        o.status = Status.Cancelled;
        emit ObligationCancelled(id);
    }

    function expire(bytes32 id) external {
        Obligation storage o = obligations[id];
        if (o.status != Status.Queued) revert NotQueued(id, o.status);
        if (block.timestamp <= o.expiresAt) revert NotYetExpired(id, o.expiresAt);
        o.status = Status.Expired;
        emit ObligationExpired(id);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                Netting cycles
    //////////////////////////////////////////////////////////////////////////*/

    /// @notice Settles a netting cycle. `members` and `discharged` must be
    ///         strictly increasing; `nets[i]` is members[i]'s net position
    ///         (positive receives). Every net is recomputed from the
    ///         obligations before the ledger moves anything.
    function settleCycle(bytes32 cycleId, bytes32[] calldata members, int256[] calldata nets, bytes32[] calldata discharged)
        external
        onlyRole(OPERATOR_ROLE)
        whenNotPaused
    {
        if (cycleSettled[cycleId]) revert CycleAlreadySettled(cycleId);
        if (members.length != nets.length) revert LengthMismatch();
        for (uint256 i = 1; i < members.length; i++) {
            if (members[i] <= members[i - 1]) revert NotStrictlyIncreasing(i);
        }
        cycleSettled[cycleId] = true;

        int256[] memory computed = new int256[](members.length);
        uint256 gross;
        for (uint256 k = 0; k < discharged.length; k++) {
            if (k > 0 && discharged[k] <= discharged[k - 1]) revert NotStrictlyIncreasing(k);
            Obligation storage o = _live(discharged[k]);
            o.status = Status.Settled;
            gross += o.amount;
            computed[_indexOf(members, o.payer)] -= int256(uint256(o.amount));
            computed[_indexOf(members, o.payee)] += int256(uint256(o.amount));
        }

        uint256 net;
        for (uint256 i = 0; i < members.length; i++) {
            if (computed[i] != nets[i]) revert NetMismatch(members[i], nets[i], computed[i]);
            if (nets[i] < 0) net += uint256(-nets[i]);
        }

        LEDGER.applyNet(members, nets);
        grossDischarged += gross;
        netMoved += net;
        for (uint256 k = 0; k < discharged.length; k++) {
            emit ObligationSettled(discharged[k], cycleId);
        }
        emit CycleSettled(cycleId, discharged.length, gross, net);
    }

    /// @notice Value discharged per unit moved, in bps (290000 = 29:1).
    ///         A cycle that discharges value while moving nothing is the best
    ///         possible outcome and reads as the maximum, never as zero.
    function liquidityEfficiencyBps() external view returns (uint256) {
        if (netMoved == 0) return grossDischarged > 0 ? type(uint256).max : 0;
        return (grossDischarged * 10_000) / netMoved;
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Admin
    //////////////////////////////////////////////////////////////////////////*/

    function setTtl(uint32 t) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (t < MIN_TTL || t > MAX_TTL) revert BadTtl(t);
        ttl = t;
        emit TtlChanged(t);
    }

    function pause() external onlyRole(PAUSER_ROLE) {
        _pause();
    }

    function unpause() external onlyRole(PAUSER_ROLE) {
        _unpause();
    }

    /// @notice The id a payer's reference will produce.
    function obligationIdFor(bytes32 payer, bytes32 clientRef) external view returns (bytes32) {
        return keccak256(abi.encode(block.chainid, address(this), payer, clientRef));
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Internals
    //////////////////////////////////////////////////////////////////////////*/

    function _onlyOperatorOf(bytes32 memberId) internal view {
        OmnibusLedger.Member memory m = LEDGER.member(memberId);
        if (!m.admitted) revert UnknownMember(memberId);
        if (msg.sender != m.operator) revert NotTheOperator(memberId, msg.sender);
    }

    function _live(bytes32 id) internal view returns (Obligation storage o) {
        o = obligations[id];
        if (o.status != Status.Queued) revert NotQueued(id, o.status);
        if (block.timestamp > o.expiresAt) revert ObligationHasExpired(id, o.expiresAt);
    }

    /// @dev Binary search: members is strictly increasing.
    function _indexOf(bytes32[] calldata members, bytes32 id) internal pure returns (uint256) {
        uint256 lo = 0;
        uint256 hi = members.length;
        while (lo < hi) {
            uint256 mid = (lo + hi) / 2;
            if (members[mid] == id) return mid;
            if (members[mid] < id) lo = mid + 1;
            else hi = mid;
        }
        revert UnknownMemberInNetSet(id);
    }

}
